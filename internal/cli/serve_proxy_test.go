package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIDKeyNormalisation(t *testing.T) {
	t.Parallel()
	equal := [][2]string{
		{`1`, ` 1 `},   // whitespace-insensitive
		{`"x"`, `"x"`}, // string ids
		{`42`, `42`},   // integer ids
		{`1.0`, `1`},   // numerically equal floats normalise together
	}
	for _, c := range equal {
		if got, want := idKey([]byte(c[0])), idKey([]byte(c[1])); got != want {
			t.Errorf("idKey(%q)=%q, idKey(%q)=%q — want equal", c[0], got, c[1], want)
		}
	}
	if idKey([]byte(`1`)) == idKey([]byte(`"1"`)) {
		t.Errorf("number 1 and string \"1\" must not share a key")
	}
}

func TestEnvelopeClassification(t *testing.T) {
	t.Parallel()
	req := parseEnvelope([]byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call"}`))
	if !req.isRequest() || req.isResponse() {
		t.Errorf("request misclassified: %+v", req)
	}
	resp := parseEnvelope([]byte(`{"jsonrpc":"2.0","id":5,"result":{}}`))
	if resp.isRequest() || !resp.isResponse() {
		t.Errorf("response misclassified: %+v", resp)
	}
	note := parseEnvelope([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if note.isRequest() || note.isResponse() {
		t.Errorf("notification misclassified: %+v", note)
	}
}

// mockDaemon serves the daemon side of a net.Pipe: it answers initialize, ping,
// and generic requests. Behaviour is controllable to simulate crash and hang.
type mockDaemon struct {
	conn          net.Conn     // daemon side of the pipe
	hangPing      *atomic.Bool // when true, ping requests get no reply (hung)
	crashOnTool   bool         // close the connection on the first tool call (crash)
	mcpToolResult bool         // answer tool calls with an MCP content-array result shape
	version       string       // serverInfo.version in the initialize reply; default "1.0.0-mock"
	noServerInfo  bool         // emit the legacy initialize shape without serverInfo
}

func startMockDaemon(m *mockDaemon) {
	go func() {
		fr := newFrameReader(m.conn)
		for {
			frame, err := fr.read()
			if err != nil {
				return
			}
			e := parseEnvelope(frame)
			switch {
			case e.Method == "initialize":
				if m.noServerInfo {
					_ = writeFrame(m.conn, fmt.Appendf(nil,
						`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05"}}`, e.ID))
					continue
				}
				v := m.version
				if v == "" {
					v = "1.0.0-mock"
				}
				_ = writeFrame(m.conn, fmt.Appendf(nil,
					`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"plumb-mock","version":%q}}}`, e.ID, v))
			case e.Method == "notifications/initialized":
				// notification — no reply
			case e.Method == "ping":
				if m.hangPing == nil || !m.hangPing.Load() {
					_ = writeFrame(m.conn, fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,"result":{}}`, e.ID))
				}
			case e.isRequest():
				if m.crashOnTool {
					_ = m.conn.Close()
					return
				}
				if m.mcpToolResult {
					_ = writeFrame(m.conn, fmt.Appendf(nil,
						`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`, e.ID))
					continue
				}
				_ = writeFrame(m.conn, fmt.Appendf(nil,
					`{"jsonrpc":"2.0","id":%s,"result":{"method":%q}}`, e.ID, e.Method))
			}
		}
	}()
}

// newPipeDaemon returns a started mockDaemon and the proxy-facing end of its pipe.
func newPipeDaemon(opts func(*mockDaemon)) (*mockDaemon, net.Conn) {
	proxySide, daemonSide := net.Pipe()
	m := &mockDaemon{conn: daemonSide}
	if opts != nil {
		opts(m)
	}
	startMockDaemon(m)
	return m, proxySide
}

// proxyHarness wires a reconnectingProxy to in-memory client pipes and a queue
// of replacement daemon connections handed out by the dial hook.
type proxyHarness struct {
	t         *testing.T
	clientIn  *io.PipeWriter
	clientOut *frameReader
	dialQueue chan net.Conn
	killCount *atomic.Int32
	proxy     *reconnectingProxy
}

func startProxy(t *testing.T, initialProxySide net.Conn, hb, pingTO time.Duration) *proxyHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &proxyHarness{
		t:         t,
		clientIn:  inW,
		clientOut: newFrameReader(outR),
		dialQueue: make(chan net.Conn, 8),
		killCount: &atomic.Int32{},
	}
	h.proxy = newReconnectingProxy(proxyDeps{
		in:      inR,
		out:     outW,
		initial: initialProxySide,
		dial: func(context.Context) (net.Conn, error) {
			select {
			case c := <-h.dialQueue:
				return c, nil
			default:
				return nil, errors.New("no more daemons available")
			}
		},
		killDaemon:        func(int) { h.killCount.Add(1) },
		heartbeatInterval: hb,
		pingTimeout:       pingTO,
		maxReconnects:     3,
		baseBackoff:       time.Millisecond,
		// Generous waits: these fire only on failure, but a tight deadline
		// flakes under parallel-suite machine load (observed at 2–3 s).
		handshakeWait: 10 * time.Second,
	})
	return h
}

func (h *proxyHarness) start() <-chan error {
	done := make(chan error, 1)
	go func() { done <- h.proxy.run(context.Background()) }()
	return done
}

func (h *proxyHarness) write(frame string) {
	h.t.Helper()
	if err := writeFrame(h.clientIn, []byte(frame)); err != nil {
		h.t.Fatalf("writing client frame: %v", err)
	}
}

func (h *proxyHarness) read(d time.Duration) string {
	h.t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() { b, err := h.clientOut.read(); ch <- res{b, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			h.t.Fatalf("reading client frame: %v", r.err)
		}
		return string(r.b)
	case <-time.After(d):
		h.t.Fatalf("timed out waiting for a client frame")
		return ""
	}
}

func (h *proxyHarness) handshake() {
	h.t.Helper()
	h.write(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if got := h.read(10 * time.Second); !strings.Contains(got, `"id":1`) || !strings.Contains(got, "protocolVersion") {
		h.t.Fatalf("expected initialize response, got %q", got)
	}
	h.write(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

func TestProxyCrashRecovery(t *testing.T) {
	t.Parallel()

	// Initial daemon crashes on the first tool call, leaving it in flight.
	_, initialProxySide := newPipeDaemon(func(m *mockDaemon) { m.crashOnTool = true })
	h := startProxy(t, initialProxySide, 0, 0)

	// A healthy replacement waiting for the dial hook.
	_, replProxySide := newPipeDaemon(nil)
	h.dialQueue <- replProxySide

	h.start()
	h.handshake()

	// In-flight tool call; the initial daemon crashes instead of answering.
	h.write(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`)

	// After reconnect + handshake replay, the in-flight request gets a synthesised
	// retryable error. The replayed initialize response is swallowed, so id 1 does
	// not reappear ahead of it.
	frame := h.read(10 * time.Second)
	if !strings.Contains(frame, `"id":5`) || !strings.Contains(frame, "daemon restarted") {
		t.Fatalf("expected synthesised error for in-flight id 5, got %q", frame)
	}

	// The session is live again — a fresh tool call is served by the replacement.
	h.write(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{}}`)
	frame = h.read(10 * time.Second)
	if !strings.Contains(frame, `"id":6`) || !strings.Contains(frame, `"result"`) {
		t.Fatalf("expected result for id 6 from replacement daemon, got %q", frame)
	}
	_ = h.clientIn.Close()
}

// TestProxyReconnectNote verifies the one-shot daemon-reconnected note: after a
// transparent reconnect, the FIRST tools/call result carries the note, and the
// next one does not.
func TestProxyReconnectNote(t *testing.T) {
	t.Parallel()

	_, initialProxySide := newPipeDaemon(func(m *mockDaemon) { m.crashOnTool = true })
	h := startProxy(t, initialProxySide, 0, 0)
	_, replProxySide := newPipeDaemon(func(m *mockDaemon) {
		m.mcpToolResult = true
		m.version = "2.0.0-repl" // distinct from the initial daemon AND the proxy's Version ("dev" in tests)
	})
	h.dialQueue <- replProxySide

	h.start()
	h.handshake()

	// Trigger the crash + reconnect; the in-flight id 5 gets the retryable error.
	h.write(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`)
	if frame := h.read(10 * time.Second); !strings.Contains(frame, `"id":5`) || !strings.Contains(frame, "daemon restarted") {
		t.Fatalf("expected synthesised error for in-flight id 5, got %q", frame)
	}

	// First tools/call after the reconnect: result carries the one-shot note,
	// appended as an extra content item (the original "ok" text is preserved).
	// The note must report the REPLACEMENT daemon's serverInfo.version — not
	// this (older) proxy binary's own compiled Version.
	h.write(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{}}`)
	frame := h.read(10 * time.Second)
	if !strings.Contains(frame, `"id":6`) || !strings.Contains(frame, "daemon reconnected") {
		t.Fatalf("expected reconnect note on the first tool call after reconnect, got %q", frame)
	}
	if !strings.Contains(frame, `"ok"`) {
		t.Fatalf("the original tool result content must be preserved, got %q", frame)
	}
	if !strings.Contains(frame, "daemon now 2.0.0-repl") {
		t.Fatalf("note must carry the new daemon's serverInfo.version, got %q", frame)
	}
	if strings.Contains(frame, "now "+Version+")") {
		t.Fatalf("note must not claim the proxy's own version as the daemon's, got %q", frame)
	}
	if !strings.Contains(frame, "this serve proxy is still "+Version) {
		t.Fatalf("differing versions must surface the proxy lag, got %q", frame)
	}

	// Second tools/call: no note (strictly one-shot).
	h.write(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{}}`)
	if frame := h.read(10 * time.Second); strings.Contains(frame, "daemon reconnected") {
		t.Fatalf("reconnect note must be one-shot, but a second tool call also carried it: %q", frame)
	}
	_ = h.clientIn.Close()
}

func TestInjectReconnectNote(t *testing.T) {
	t.Parallel()

	// Well-formed tools/call result: note appended, original content preserved.
	good := []byte(`{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"hello"}]}}`)
	out, ok := injectReconnectNote(good, "v9.9.9", "v9.9.9")
	if !ok {
		t.Fatal("expected injection into a well-formed tools/call result")
	}
	s := string(out)
	if !strings.Contains(s, "hello") || !strings.Contains(s, "daemon reconnected") || !strings.Contains(s, "v9.9.9") {
		t.Fatalf("expected note appended alongside original content, got %q", s)
	}

	// Fail-safe shapes: each returns the input unchanged with ok=false.
	for _, c := range []struct {
		name  string
		frame string
	}{
		{"error response", `{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"x"}}`},
		{"result without content", `{"jsonrpc":"2.0","id":3,"result":{"method":"x"}}`},
		{"not json", `not json at all`},
		{"content not array", `{"jsonrpc":"2.0","id":3,"result":{"content":"oops"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := injectReconnectNote([]byte(c.frame), "v1", "v1")
			if ok {
				t.Fatalf("expected ok=false for %s", c.name)
			}
			if string(got) != c.frame {
				t.Fatalf("a refused injection must return the frame unchanged, got %q", got)
			}
		})
	}
}

func TestReconnectNoteText(t *testing.T) {
	t.Parallel()

	// Same versions: plain note, no proxy-lag hint.
	same := reconnectNoteText("1.2.3", "1.2.3")
	if !strings.Contains(same, "(now 1.2.3)") || strings.Contains(same, "serve proxy") {
		t.Errorf("same-version note wrong: %q", same)
	}
	// Unknown daemon version: fall back to the proxy's, no lag hint.
	fallback := reconnectNoteText("", "1.2.3")
	if !strings.Contains(fallback, "(now 1.2.3)") || strings.Contains(fallback, "serve proxy") {
		t.Errorf("fallback note wrong: %q", fallback)
	}
	// Differing versions: daemon's version leads, proxy lag stated.
	differ := reconnectNoteText("2.0.0", "1.2.3")
	if !strings.Contains(differ, "daemon now 2.0.0") ||
		!strings.Contains(differ, "this serve proxy is still 1.2.3") ||
		!strings.Contains(differ, "start a new client session") {
		t.Errorf("differ note wrong: %q", differ)
	}
}

func TestServerInfoVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		frame string
		want  string
	}{
		{"well-formed", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"plumb","version":"0.9.17"}}}`, "0.9.17"},
		{"missing serverInfo", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`, ""},
		{"error response", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"x"}}`, ""},
		{"malformed json", `not json`, ""},
	}
	for _, c := range cases {
		if got := serverInfoVersion([]byte(c.frame)); got != c.want {
			t.Errorf("%s: serverInfoVersion = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestProxyReconnectNote_LegacyDaemonShape: a daemon whose initialize response
// carries no serverInfo (the pre-serverInfo shape) still yields a note — the
// version falls back to the proxy's own, with no lag hint.
func TestProxyReconnectNote_LegacyDaemonShape(t *testing.T) {
	t.Parallel()

	_, initialProxySide := newPipeDaemon(func(m *mockDaemon) {
		m.crashOnTool = true
		m.noServerInfo = true
	})
	h := startProxy(t, initialProxySide, 0, 0)
	_, replProxySide := newPipeDaemon(func(m *mockDaemon) {
		m.mcpToolResult = true
		m.noServerInfo = true
	})
	h.dialQueue <- replProxySide

	h.start()
	h.handshake()

	h.write(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`)
	if frame := h.read(10 * time.Second); !strings.Contains(frame, `"id":5`) {
		t.Fatalf("expected synthesised error for in-flight id 5, got %q", frame)
	}

	h.write(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{}}`)
	frame := h.read(10 * time.Second)
	if !strings.Contains(frame, "daemon reconnected") || !strings.Contains(frame, "(now "+Version+")") {
		t.Fatalf("legacy shape should fall back to the proxy version, got %q", frame)
	}
	if strings.Contains(frame, "serve proxy is still") {
		t.Fatalf("fallback must not claim a version lag, got %q", frame)
	}
	_ = h.clientIn.Close()
}

// TestProxyReconnectNote_ModernThenLegacy: a modern daemon (one that reports a
// serverInfo.version) is replaced on reconnect by a legacy one (no serverInfo).
// The note must fall back to the proxy's own version, NOT keep reporting the
// dead modern daemon's stale version. Regression for consumeInitializeResponse
// only capturing the version when non-empty (which left the modern version
// cached across a modern→legacy replacement).
func TestProxyReconnectNote_ModernThenLegacy(t *testing.T) {
	t.Parallel()

	_, initialProxySide := newPipeDaemon(func(m *mockDaemon) {
		m.crashOnTool = true
		m.version = "2.0.0-orig" // modern initial daemon: reports a version
	})
	h := startProxy(t, initialProxySide, 0, 0)
	_, replProxySide := newPipeDaemon(func(m *mockDaemon) {
		m.mcpToolResult = true
		m.noServerInfo = true // legacy replacement: no serverInfo at all
	})
	h.dialQueue <- replProxySide

	h.start()
	h.handshake() // captures the modern daemon's "2.0.0-orig"

	h.write(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`)
	if frame := h.read(10 * time.Second); !strings.Contains(frame, `"id":5`) {
		t.Fatalf("expected synthesised error for in-flight id 5, got %q", frame)
	}

	h.write(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{}}`)
	frame := h.read(10 * time.Second)
	if !strings.Contains(frame, "daemon reconnected") {
		t.Fatalf("expected reconnect note, got %q", frame)
	}
	if !strings.Contains(frame, "(now "+Version+")") {
		t.Fatalf("a legacy replacement must fall back to the proxy version, got %q", frame)
	}
	if strings.Contains(frame, "2.0.0-orig") {
		t.Fatalf("note must not report the dead modern daemon's stale version, got %q", frame)
	}
	if strings.Contains(frame, "serve proxy is still") {
		t.Fatalf("fallback must not claim a version lag, got %q", frame)
	}
	_ = h.clientIn.Close()
}

// TestTrackOutstanding_LateTrackAfterReconnect: the daemon dies — and the
// reconnect sweep runs — in the gap between a successful write and
// trackOutstanding's store (track-after-write). The post-store generation
// check must synthesise the retryable error rather than orphan the request:
// before the fix the client hung forever, surfacing as the proxy-test
// family's long-standing "timed out waiting for a client frame" load flake.
func TestTrackOutstanding_LateTrackAfterReconnect(t *testing.T) {
	t.Parallel()

	outR, outW := io.Pipe()
	clientOut := newFrameReader(outR)
	c1, _ := net.Pipe()
	p := newReconnectingProxy(proxyDeps{out: outW, initial: c1})

	// A reconnect completed between the write (gen 1) and the track: the
	// generation is now 2 and the sweep ran while the entry was absent.
	c2, _ := net.Pipe()
	_ = p.publish(c2, newFrameReader(c2)) // gen 2
	p.failOutstandingBelow(2)             // the reconnect sweep — sees nothing

	done := make(chan string, 1)
	go func() {
		b, err := clientOut.read()
		if err != nil {
			done <- "read error: " + err.Error()
			return
		}
		done <- string(b)
	}()

	// The late track must detect the stale generation and synthesise the error.
	p.trackOutstanding([]byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`), 1)

	select {
	case frame := <-done:
		if !strings.Contains(frame, `"id":5`) || !strings.Contains(frame, "daemon restarted") {
			t.Fatalf("late-tracked request must get the synthesised error, got %q", frame)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("late-tracked request was orphaned — no synthesised error arrived")
	}
}

// TestProxySurvivesProlongedOutageThenRecovers is the headline reliability
// contract: when the daemon stays unreachable past the fast retry budget (a
// restart/upgrade window), the proxy must NOT exit — exiting would close the
// client's stdio and make the host de-register every plumb tool for the rest of
// the session. Instead it fails the in-flight request with a retryable error
// (so the client is not left hanging), keeps the client connection alive, and
// retries in the background; the next tool call succeeds once a daemon returns.
func TestProxySurvivesProlongedOutageThenRecovers(t *testing.T) {
	t.Parallel()

	_, initialProxySide := newPipeDaemon(func(m *mockDaemon) { m.crashOnTool = true })
	h := startProxy(t, initialProxySide, 0, 0)
	// dialQueue starts empty → reconnects fail through the fast phase.
	done := h.start()
	h.handshake()
	h.write(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`) // triggers the crash

	// The in-flight request gets the retryable error once the fast phase is
	// exhausted — not a hang for the whole outage.
	frame := h.read(10 * time.Second)
	if !strings.Contains(frame, `"id":5`) || !strings.Contains(frame, "daemon restarted") {
		t.Fatalf("in-flight request must get the synthesised retryable error, got %q", frame)
	}
	// run() must NOT have returned — the client connection stays alive.
	select {
	case err := <-done:
		t.Fatalf("proxy exited during a daemon outage (it must stay alive): %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// A daemon returns; the next tool call must succeed against it.
	_, replProxySide := newPipeDaemon(nil)
	h.dialQueue <- replProxySide
	h.write(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{}}`)
	frame = h.read(10 * time.Second)
	if !strings.Contains(frame, `"id":6`) || !strings.Contains(frame, `"result"`) {
		t.Fatalf("expected result for id 6 after recovery, got %q", frame)
	}

	// Clean shutdown: closing client stdin ends run() with nil.
	_ = h.clientIn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean shutdown should return nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not shut down after the client closed stdin")
	}
}

func TestProxyHangDetection(t *testing.T) {
	t.Parallel()

	var hang atomic.Bool
	hang.Store(true) // initial daemon never answers ping → looks hung
	_, initialProxySide := newPipeDaemon(func(m *mockDaemon) { m.hangPing = &hang })
	h := startProxy(t, initialProxySide, 30*time.Millisecond, 80*time.Millisecond)

	_, replProxySide := newPipeDaemon(nil) // healthy replacement
	h.dialQueue <- replProxySide

	h.start()
	h.handshake()

	// The hung daemon should be killed and replaced.
	deadline := time.After(10 * time.Second)
	for h.killCount.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("hung daemon was not detected/killed within the deadline")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// A fresh tool call now succeeds against the replacement.
	h.write(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{}}`)
	frame := h.read(10 * time.Second)
	if !strings.Contains(frame, `"id":7`) || !strings.Contains(frame, `"result"`) {
		t.Fatalf("expected result for id 7 after hang recovery, got %q", frame)
	}
	_ = h.clientIn.Close()
}

// TestProxyOutstandingTrackedOnlyAfterSend is the F11 regression: a request must
// be tracked as in-flight only AFTER its write to the daemon succeeds. If it were
// tracked before the write, a reconnect triggered by that very write's failure
// would both synthesise a -32000 for it AND let the pump re-send it to the fresh
// daemon — a double response and a forbidden auto-replay.
func TestProxyOutstandingTrackedOnlyAfterSend(t *testing.T) {
	t.Parallel()
	p := newReconnectingProxy(proxyDeps{})

	count := func() int {
		p.reqMu.Lock()
		defer p.reqMu.Unlock()
		return len(p.outstanding)
	}
	has := func(raw string) bool {
		p.reqMu.Lock()
		defer p.reqMu.Unlock()
		_, ok := p.outstanding[idKey([]byte(raw))]
		return ok
	}

	toolCall := []byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call"}`)
	p.captureHandshake(toolCall)
	if count() != 0 {
		t.Fatalf("captureHandshake tracked %d outstanding; want 0 (track only after a successful send)", count())
	}
	p.trackOutstanding(toolCall, p.generation())
	if !has("5") {
		t.Error("trackOutstanding did not record the sent request id 5")
	}

	// The initialize request is replayed by replayHandshake, never failed, so it
	// must not be tracked as outstanding.
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	p.captureHandshake(initFrame)
	p.trackOutstanding(initFrame, p.generation())
	if has("1") {
		t.Error("initialize must not be tracked as outstanding")
	}
}

// TestProxyKillTargetsCapturedPID is the F12 regression: the hang-kill path must
// terminate the exact daemon PID the proxy connected to (captured at connect),
// not whatever the PID file holds at kill time — otherwise a freshly-respawned
// daemon could be killed by a different client's hang detection.
func TestProxyKillTargetsCapturedPID(t *testing.T) {
	t.Parallel()
	var killed int
	p := newReconnectingProxy(proxyDeps{
		dial:          func(context.Context) (net.Conn, error) { return nil, errors.New("no daemon") },
		killDaemon:    func(pid int) { killed = pid },
		maxReconnects: 1,
		baseBackoff:   time.Millisecond,
	})
	p.daemonPID.Store(4242) // override the construction-time PID-file read
	// The kill fires before the (now-unbounded) retry loop, so a pre-cancelled
	// context lets reconnect terminate deterministically once the kill has run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = p.reconnect(ctx, p.generation(), true) // kill=true → hang path
	if killed != 4242 {
		t.Errorf("kill targeted pid %d; want the captured 4242", killed)
	}
}

// TestProxyReconnectStaleGenerationDoesNotKill guards the invariant the
// heartbeat relies on by capturing the generation BEFORE the ping: a reconnect
// request for a generation the proxy has already moved past must be a no-op and
// must NOT kill the daemon. Otherwise a hang verdict reached after a concurrent
// reconnect would SIGKILL the freshly-respawned, healthy daemon.
func TestProxyReconnectStaleGenerationDoesNotKill(t *testing.T) {
	t.Parallel()
	var killed bool
	p := newReconnectingProxy(proxyDeps{
		dial:          func(context.Context) (net.Conn, error) { return nil, errors.New("no daemon") },
		killDaemon:    func(int) { killed = true },
		maxReconnects: 1,
		baseBackoff:   time.Millisecond,
	})
	staleGen := p.generation()
	p.publish(nil, nil) // a concurrent reconnect advanced the generation past staleGen
	if err := p.reconnect(context.Background(), staleGen, true); err != nil {
		t.Fatalf("stale-generation reconnect should be a no-op, got error: %v", err)
	}
	if killed {
		t.Error("reconnect for a stale generation killed the daemon; the heartbeat would then SIGKILL a healthy respawned daemon")
	}
}
