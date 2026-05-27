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
	conn        net.Conn     // daemon side of the pipe
	hangPing    *atomic.Bool // when true, ping requests get no reply (hung)
	crashOnTool bool         // close the connection on the first tool call (crash)
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
				_ = writeFrame(m.conn, fmt.Appendf(nil,
					`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05"}}`, e.ID))
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
		handshakeWait:     2 * time.Second,
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
	if got := h.read(2 * time.Second); !strings.Contains(got, `"id":1`) || !strings.Contains(got, "protocolVersion") {
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
	frame := h.read(3 * time.Second)
	if !strings.Contains(frame, `"id":5`) || !strings.Contains(frame, "daemon restarted") {
		t.Fatalf("expected synthesised error for in-flight id 5, got %q", frame)
	}

	// The session is live again — a fresh tool call is served by the replacement.
	h.write(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{}}`)
	frame = h.read(3 * time.Second)
	if !strings.Contains(frame, `"id":6`) || !strings.Contains(frame, `"result"`) {
		t.Fatalf("expected result for id 6 from replacement daemon, got %q", frame)
	}
	_ = h.clientIn.Close()
}

func TestProxyGivesUpAfterMaxReconnects(t *testing.T) {
	t.Parallel()

	_, initialProxySide := newPipeDaemon(func(m *mockDaemon) { m.crashOnTool = true })
	h := startProxy(t, initialProxySide, 0, 0)
	// dialQueue stays empty → every reconnect attempt fails.
	done := h.start()
	h.handshake()
	h.write(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`) // triggers the crash

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("expected give-up error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not give up within the deadline")
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
	deadline := time.After(3 * time.Second)
	for h.killCount.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("hung daemon was not detected/killed within the deadline")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// A fresh tool call now succeeds against the replacement.
	h.write(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{}}`)
	frame := h.read(3 * time.Second)
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
	p.trackOutstanding(toolCall)
	if !has("5") {
		t.Error("trackOutstanding did not record the sent request id 5")
	}

	// The initialize request is replayed by replayHandshake, never failed, so it
	// must not be tracked as outstanding.
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	p.captureHandshake(initFrame)
	p.trackOutstanding(initFrame)
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
	p.daemonPID.Store(4242)                                     // override the construction-time PID-file read
	_ = p.reconnect(context.Background(), p.generation(), true) // kill=true → hang path
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
