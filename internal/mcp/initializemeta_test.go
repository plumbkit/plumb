package mcp

// initializemeta_test.go — the additive `_meta` an application contributes to
// the initialize RESULT.
//
// The handshake is the one exchange every MCP connection performs exactly once,
// before any tool is served, which is why identity travels on it (PLAN-426).
// Two properties make that safe to add to a protocol message every client
// parses: an unwired hook changes the response not at all, and the hook runs
// after the param hooks so the application can report what it learned from them.

import (
	"context"
	"encoding/json"
	"testing"
)

// initResult decodes an initialize response's result object.
func initResult(t *testing.T, resp mcpResponse) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshalling the initialize result: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding the initialize result: %v", err)
	}
	return out
}

// TestInitializeMeta_AbsentWhenUnwired is the compatibility property the whole
// additive design rests on: a build with the hook, and no hook set, emits a
// response indistinguishable from one that predates the hook entirely.
func TestInitializeMeta_AbsentWhenUnwired(t *testing.T) {
	s := New(ServerInfo{Name: "plumb", Version: "1.2.3"})
	got := initResult(t, s.handleInitialize(context.Background(), mcpRequest{ID: 1, Params: json.RawMessage(`{}`)}))
	if _, present := got["_meta"]; present {
		t.Fatalf("an unwired server emitted _meta: %v", got)
	}

	// An empty map is the hook declining, and must be indistinguishable from
	// not being wired at all — otherwise a caller that has nothing to say still
	// changes the wire.
	s.InitializeMeta = func(context.Context) map[string]any { return map[string]any{} }
	got = initResult(t, s.handleInitialize(context.Background(), mcpRequest{ID: 1, Params: json.RawMessage(`{}`)}))
	if _, present := got["_meta"]; present {
		t.Fatalf("a declining hook emitted _meta: %v", got)
	}
}

func TestInitializeMeta_CarriesTheApplicationsKeys(t *testing.T) {
	s := New(ServerInfo{Name: "plumb", Version: "1.2.3"})
	s.InitializeMeta = func(context.Context) map[string]any {
		return map[string]any{
			MetaSessionIdentityKey: map[string]any{"session_id": "sess-1", "recovery": "restored"},
			MetaDaemonInstanceKey:  "daemon-a",
		}
	}
	got := initResult(t, s.handleInitialize(context.Background(), mcpRequest{ID: 1, Params: json.RawMessage(`{}`)}))

	var meta map[string]json.RawMessage
	if err := json.Unmarshal(got["_meta"], &meta); err != nil {
		t.Fatalf("decoding _meta: %v", err)
	}
	if _, ok := meta[MetaSessionIdentityKey]; !ok {
		t.Errorf("_meta lacks %s: %v", MetaSessionIdentityKey, meta)
	}
	var instance string
	if err := json.Unmarshal(meta[MetaDaemonInstanceKey], &instance); err != nil || instance != "daemon-a" {
		t.Errorf("_meta[%s] = %q (%v), want daemon-a", MetaDaemonInstanceKey, instance, err)
	}
	// The rest of the result is untouched — this is additive, not a rewrite.
	var protocol string
	if err := json.Unmarshal(got["protocolVersion"], &protocol); err != nil || protocol == "" {
		t.Errorf("protocolVersion = %q (%v); adding _meta must not disturb the negotiated result", protocol, err)
	}
}

// TestInitializeMeta_RunsAfterTheParamHooks pins the ordering the identity
// snapshot depends on. The param hooks are what tell the application which proxy
// session this connection is; a snapshot taken before them would state an
// identity that had not yet been recovered.
func TestInitializeMeta_RunsAfterTheParamHooks(t *testing.T) {
	s := New(ServerInfo{Name: "plumb", Version: "1.2.3"})
	var recovered string
	s.OnProxySession = func(_ context.Context, id string) { recovered = "identity-for-" + id }
	s.InitializeMeta = func(context.Context) map[string]any {
		return map[string]any{MetaSessionIdentityKey: recovered}
	}

	params := json.RawMessage(`{"_meta":{"dev.plumbkit/proxy-session-id":"proxyX"}}`)
	got := initResult(t, s.handleInitialize(context.Background(), mcpRequest{ID: 1, Params: params}))

	var meta struct {
		Identity string `json:"dev.plumbkit/session-identity"`
	}
	if err := json.Unmarshal(got["_meta"], &meta); err != nil {
		t.Fatalf("decoding _meta: %v", err)
	}
	if meta.Identity != "identity-for-proxyX" {
		t.Fatalf("identity = %q, want the value established by the param hook — the snapshot ran "+
			"before the hook that resolves it", meta.Identity)
	}
}
