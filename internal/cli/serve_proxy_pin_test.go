package cli

// serve_proxy_pin_test.go — the proxy's memory of the last explicit re-pin.
//
// The `plumb serve` proxy is the process that SURVIVES a daemon restart, so it
// is the natural keeper of "which workspace did the caller actually choose".
// It watches for a successful session_start carrying a workspace argument and
// replays that workspace in the initialize _meta on every reconnect, which is
// literally rung 1 of the attach ladder (see conn_attach_oninit.go).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

func sessionStartFrame(id, workspace string) []byte {
	if workspace == "" {
		return []byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call",` +
			`"params":{"name":"session_start","arguments":{}}}`)
	}
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call",` +
		`"params":{"name":"session_start","arguments":{"workspace":"` + workspace + `"}}}`)
}

func okResult(id string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"content":[{"type":"text","text":"# Workspace: /x"}]}}`)
}

func errResult(id string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"isError":true,"content":[{"type":"text","text":"no such dir"}]}}`)
}

func rpcError(id string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-32603,"message":"boom"}}`)
}

func newPinProxy() *reconnectingProxy {
	return &reconnectingProxy{pending: map[string]string{}}
}

func TestObserveSessionStart_RecordsWorkspaceOnSuccess(t *testing.T) {
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("7", "/Users/me/proj"))
	if got := p.pinnedWorkspace(); got != "" {
		t.Fatalf("pin committed before the daemon answered: %q", got)
	}
	p.commitSessionStartPin(okResult("7"))
	if got := p.pinnedWorkspace(); got != "/Users/me/proj" {
		t.Fatalf("pinnedWorkspace = %q, want /Users/me/proj", got)
	}
}

func TestObserveSessionStart_NotCommittedOnErrorResult(t *testing.T) {
	// A rejected re-pin (bad path) must not stick: replaying it would re-pin the
	// fresh daemon to a workspace the live one refused.
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("7", "/nope"))
	p.commitSessionStartPin(errResult("7"))
	if got := p.pinnedWorkspace(); got != "" {
		t.Fatalf("pin committed from an isError result: %q", got)
	}
}

func TestObserveSessionStart_NotCommittedOnRPCError(t *testing.T) {
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("7", "/nope"))
	p.commitSessionStartPin(rpcError("7"))
	if got := p.pinnedWorkspace(); got != "" {
		t.Fatalf("pin committed from a JSON-RPC error: %q", got)
	}
}

func TestObserveSessionStart_NoWorkspaceArgDoesNotClear(t *testing.T) {
	// session_start without a workspace is a plain orientation call. It must never
	// enter the pending map, so it cannot clear a pin the caller already chose.
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("1", "/Users/me/proj"))
	p.commitSessionStartPin(okResult("1"))

	p.observeClientRequest(sessionStartFrame("2", ""))
	p.commitSessionStartPin(okResult("2"))

	if got := p.pinnedWorkspace(); got != "/Users/me/proj" {
		t.Fatalf("pinnedWorkspace = %q; a workspace-less session_start cleared the pin", got)
	}
}

func TestObserveSessionStart_IgnoresOtherTools(t *testing.T) {
	p := newPinProxy()
	p.observeClientRequest([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call",` +
		`"params":{"name":"read_file","arguments":{"workspace":"/elsewhere"}}}`))
	p.commitSessionStartPin(okResult("3"))
	if got := p.pinnedWorkspace(); got != "" {
		t.Fatalf("a non-session_start tool set the pin: %q", got)
	}
}

func TestObserveSessionStart_RelativeWorkspaceIgnored(t *testing.T) {
	// The replayed pin is resolved by a daemon whose cwd is unrelated to any
	// workspace. Only an absolute path is meaningful.
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("4", "relative/path"))
	p.commitSessionStartPin(okResult("4"))
	if got := p.pinnedWorkspace(); got != "" {
		t.Fatalf("a relative workspace was pinned: %q", got)
	}
}

func TestObserveSessionStart_LaterRepinReplaces(t *testing.T) {
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("1", "/a"))
	p.commitSessionStartPin(okResult("1"))
	p.observeClientRequest(sessionStartFrame("2", "/b"))
	p.commitSessionStartPin(okResult("2"))
	if got := p.pinnedWorkspace(); got != "/b" {
		t.Fatalf("pinnedWorkspace = %q, want the most recent re-pin /b", got)
	}
}

func TestPinnedWorkspaceMeta_InjectsAuthoritativeKey(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"_meta":{"keep":"me"}}}`)
	out := injectInitMeta(frame, pinnedWorkspaceMeta("/Users/me/proj"))

	if !strings.Contains(string(out), mcp.MetaPinnedWorkspaceKey) {
		t.Fatalf("replayed initialize lacks the pinned-workspace key: %s", out)
	}
	var full map[string]json.RawMessage
	if err := json.Unmarshal(out, &full); err != nil {
		t.Fatalf("replayed frame is not JSON: %v", err)
	}
	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(full["params"], &params); err != nil {
		t.Fatalf("params: %v", err)
	}
	if _, ok := params.Meta["keep"]; !ok {
		t.Fatal("injection dropped an existing _meta key")
	}
}

func TestPinnedWorkspaceMeta_EmptyPinIsByteIdentical(t *testing.T) {
	// The first connect has no pin. The replayed frame must be exactly what the
	// client sent, so a proxy that never re-pins behaves as it always did.
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if out := injectInitMeta(frame, pinnedWorkspaceMeta("")); string(out) != string(frame) {
		t.Fatalf("empty pin altered the frame:\n got %s\nwant %s", out, frame)
	}
}
