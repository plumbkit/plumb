package cli

// serve_resume_helpers_test.go — the rig the proxy-side resume-credential tests share:
// credential-shaped secrets, frames a daemon sends, a scripted daemon that sits behind
// the real proxy, and a client that records every frame the proxy hands it, because the
// property under test is about what a client can see.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// rcSecret is the n-th test credential: a valid rsk1- token (22 base64url characters).
func rcSecret(n int) string { return fmt.Sprintf("rsk1-%022d", n) }

// rcTestKey is the per-user proof key the test proxies verify against. rcOtherKey is
// another user's, which no proof made with it may satisfy.
var (
	rcTestKey  = bytes.Repeat([]byte{0x5a}, resumeProofKeyLen)
	rcOtherKey = bytes.Repeat([]byte{0xa5}, resumeProofKeyLen)
)

func rcProofKey() ([]byte, error) { return rcTestKey, nil }

// rcHooked is the arguments of a session_start as the identity hook leaves them: the
// session_id and the stamp both the conversation, and the proof of that stamp. extra is
// folded in (a workspace, say).
func rcHooked(conversation string, extra map[string]any) map[string]any {
	args := map[string]any{
		"session_id":           conversation,
		mcp.ArgLogicalAgentKey: conversation,
		mcp.ArgHookProofKey:    resumeProofFor(rcTestKey, conversation),
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// rcStore is a resume store in a fresh directory for the given daemon scope.
func rcStore(t *testing.T, dir, scope string) *resumeStore {
	t.Helper()
	s := newResumeStore(dir, scope)
	if s == nil {
		t.Fatal("newResumeStore returned nil")
	}
	return s
}

// rcMeta is a result `_meta` holding the identity snapshot a daemon sends and, when
// secret is not empty, the resume credential it discloses.
func rcMeta(recovery, secret string) map[string]any {
	meta := map[string]any{}
	if recovery != "" { // "" is a daemon that predates the identity snapshot
		meta[mcp.MetaSessionIdentityKey] = map[string]any{
			identityMetaRecovery: recovery, identityMetaSessionID: "sess-1", identityMetaName: "calm-stag", identityMetaRevision: 1,
		}
	}
	if secret != "" {
		meta[mcp.MetaResumeCredentialKey] = secret
	}
	return meta
}

func rcFrame(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rcInitResult is an initialize result disclosing secret ("" discloses nothing).
func rcInitResult(t *testing.T, id json.RawMessage, recovery, secret string) []byte {
	t.Helper()
	return rcFrame(t, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"protocolVersion": "2024-11-05",
		"serverInfo":      map[string]any{"name": "plumb", "version": "1.2.3"},
		"_meta":           rcMeta(recovery, secret),
	}})
}

// rcToolResult is a tools/call result. secret, when set, is disclosed in its `_meta`:
// the successor of an accepted resume, or a late (C3) disclosure.
func rcToolResult(t *testing.T, id json.RawMessage, text, secret string) []byte {
	t.Helper()
	res := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	if secret != "" {
		res["_meta"] = map[string]any{mcp.MetaResumeCredentialKey: secret}
	}
	return rcFrame(t, map[string]any{"jsonrpc": "2.0", "id": id, "result": res})
}

// rcDaemon is the script of an ordinary daemon: initialize discloses init under recovery,
// a session_start is answered with successor in its `_meta` ("" for none, or an error
// result when its workspace argument is "fail"), and any other tool says "ok".
func rcDaemon(t *testing.T, recovery, init, successor string) func(e rpcEnvelope, frame []byte) ([][]byte, bool) {
	t.Helper()
	return func(e rpcEnvelope, frame []byte) ([][]byte, bool) {
		switch {
		case e.Method == "initialize":
			return [][]byte{rcInitResult(t, e.ID, recovery, init)}, false
		case e.isRequest() && toolName(frame) == "session_start":
			if strings.Contains(string(frame), `"workspace":"fail"`) {
				return [][]byte{rcFrame(t, map[string]any{"jsonrpc": "2.0", "id": e.ID, "result": map[string]any{
					"isError": true, "content": []any{map[string]any{"type": "text", "text": "refused"}},
				}})}, false
			}
			return [][]byte{rcToolResult(t, e.ID, "oriented", successor)}, false
		case e.isRequest():
			return [][]byte{rcToolResult(t, e.ID, "ok", "")}, false
		}
		return nil, false
	}
}

// rcRequest is a client tools/call request.
func rcRequest(t *testing.T, id int, tool string, args, meta map[string]any) string {
	t.Helper()
	params := map[string]any{"name": tool, "arguments": args}
	if meta != nil {
		params["_meta"] = meta
	}
	return string(rcFrame(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params}))
}

// scriptedDaemon is the daemon end of a pipe that answers by a script, and records every
// frame the proxy sent it.
type scriptedDaemon struct {
	conn   net.Conn
	handle func(e rpcEnvelope, frame []byte) (replies [][]byte, closeConn bool)

	mu  sync.Mutex
	got [][]byte
}

// newScriptedDaemon starts a daemon and returns it with the proxy-facing end.
func newScriptedDaemon(handle func(e rpcEnvelope, frame []byte) ([][]byte, bool)) (*scriptedDaemon, net.Conn) {
	proxySide, daemonSide := net.Pipe()
	d := &scriptedDaemon{conn: daemonSide, handle: handle}
	go func() {
		fr := newFrameReader(daemonSide)
		for {
			frame, err := fr.read()
			if err != nil {
				return
			}
			d.mu.Lock()
			d.got = append(d.got, cloneBytes(frame))
			d.mu.Unlock()
			replies, closeConn := d.handle(parseEnvelope(frame), frame)
			for _, r := range replies {
				_ = writeFrame(daemonSide, r)
			}
			if closeConn {
				_ = daemonSide.Close()
				return
			}
		}
	}()
	return d, proxySide
}

// received is every frame the proxy has sent this daemon.
func (d *scriptedDaemon) received() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.got...)
}

// calls returns the received frames that are tools/call requests for tool.
func (d *scriptedDaemon) calls(tool string) [][]byte {
	var out [][]byte
	for _, f := range d.received() {
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if json.Unmarshal(f, &req) == nil && req.Method == "tools/call" && req.Params.Name == tool {
			out = append(out, f)
		}
	}
	return out
}

// toolName is the tool a tools/call frame names.
func toolName(frame []byte) string {
	var req struct {
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.Unmarshal(frame, &req)
	return req.Params.Name
}

// requestMeta is the params._meta of a request frame.
func requestMeta(t *testing.T, frame []byte) map[string]json.RawMessage {
	t.Helper()
	var req struct {
		Params struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &req); err != nil {
		t.Fatalf("decode request %q: %v", frame, err)
	}
	return req.Params.Meta
}

// presentedCredential is the resume credential a request frame carries, or "".
func presentedCredential(t *testing.T, frame []byte) string {
	t.Helper()
	var s string
	if raw, ok := requestMeta(t, frame)[mcp.MetaResumeCredentialKey]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// rcServe is a real reconnectingProxy with a client attached, recording every frame the
// client receives.
type rcServe struct {
	t      *testing.T
	h      *proxyHarness
	mu     sync.Mutex
	seen   []string // every frame the client received, raw
	nextID int
}

// startRCServe starts a proxy over daemons[0], queueing the rest for reconnects, with
// store as its resume store (nil for none) and the client's initialize sent.
func startRCServe(t *testing.T, store *resumeStore, first net.Conn, replacements ...net.Conn) *rcServe {
	t.Helper()
	h := startProxy(t, first, 0, 0)
	h.proxy.rc.store = store
	h.proxy.rc.proofKey = rcProofKey
	for _, r := range replacements {
		h.dialQueue <- r
	}
	h.start()
	s := &rcServe{t: t, h: h, nextID: 100}
	h.write(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	s.expectResponse(1)
	h.write(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	t.Cleanup(func() { _ = h.clientIn.Close() })
	return s
}

// expectResponse reads client frames until the response with this id, recording each.
func (s *rcServe) expectResponse(id int) string {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f := s.h.readAny(time.Until(deadline))
		s.mu.Lock()
		s.seen = append(s.seen, f)
		s.mu.Unlock()
		if e := parseEnvelope([]byte(f)); e.isResponse() && idKey(e.ID) == strconv.Itoa(id) {
			return f
		}
	}
}

// call sends a tools/call and returns the response frame.
func (s *rcServe) call(tool string, args, meta map[string]any) string {
	s.t.Helper()
	s.nextID++
	s.h.write(rcRequest(s.t, s.nextID, tool, args, meta))
	return s.expectResponse(s.nextID)
}

// drain records any further frames the client is sent within d.
func (s *rcServe) drain(d time.Duration) {
	for {
		select {
		case f, ok := <-s.h.frames:
			if !ok {
				return
			}
			s.mu.Lock()
			s.seen = append(s.seen, f)
			s.mu.Unlock()
		case <-time.After(d):
			return
		}
	}
}

// everything is every frame the client has received, joined.
func (s *rcServe) everything() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.seen, "\n")
}

// assertClientNeverSaw fails when any frame the client received names the credential
// key or holds a credential-shaped token or one of the given secrets. It refuses to
// pass on a scan that cannot see: the positive control runs first.
func (s *rcServe) assertClientNeverSaw(secrets ...string) {
	s.t.Helper()
	if !leakScanWorks() {
		s.t.Fatal("the leak scan does not fire on a credential; its silence would mean nothing")
	}
	all := s.everything()
	if strings.Contains(all, mcp.MetaResumeCredentialKey) {
		s.t.Errorf("a frame the client received names %s", mcp.MetaResumeCredentialKey)
	}
	assertNoSecret(s.t, "the frames the client received", all, secrets...)
}
