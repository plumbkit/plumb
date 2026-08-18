package cli

import (
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// okResultWithSessionID answers a session_start the way the daemon now does:
// the result _meta echoes both the canonical root and the plumb session ID.
func okResultWithSessionID(id, ws, sid string) []byte {
	frame, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.Number(id),
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
			"_meta": map[string]string{
				mcp.MetaResolvedWorkspaceKey: ws,
				mcp.MetaSessionIDKey:         sid,
			},
		},
	})
	return frame
}

func TestSessionIDCapturedAndReplayed(t *testing.T) {
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("7", "/ws"))
	p.commitSessionStartPin(okResultWithSessionID("7", "/ws", "sess-123"))

	if got := p.sessionID(); got != "sess-123" {
		t.Fatalf("sessionID = %q, want sess-123", got)
	}
	meta := p.replayInitMeta()
	var id string
	if raw, ok := meta[mcp.MetaSessionIDKey]; ok {
		_ = json.Unmarshal(raw, &id)
	}
	if id != "sess-123" {
		t.Fatalf("replayed session id = %q, want sess-123", id)
	}
	if _, ok := meta[mcp.MetaPinnedWorkspaceKey]; !ok {
		t.Fatal("replay meta is missing the pinned workspace key")
	}
}

func TestSessionIDNotCapturedWhenAbsent(t *testing.T) {
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("7", "/ws"))
	p.commitSessionStartPin(okResult("7")) // a daemon that predates the key echoes no _meta

	if got := p.sessionID(); got != "" {
		t.Fatalf("sessionID = %q, want empty when the daemon echoes none", got)
	}
	if _, ok := p.replayInitMeta()[mcp.MetaSessionIDKey]; ok {
		t.Fatal("replay meta must not carry a session id the proxy never learned")
	}
}

func TestSessionIDMetaFailSafe(t *testing.T) {
	if got := sessionIDMeta(okResultWithSessionID("1", "/ws", "sess-1")); got != "sess-1" {
		t.Fatalf("sessionIDMeta = %q, want sess-1", got)
	}
	if got := sessionIDMeta(okResult("1")); got != "" {
		t.Fatalf("sessionIDMeta without the key = %q, want empty", got)
	}
	if got := sessionIDMeta([]byte("not json")); got != "" {
		t.Fatalf("sessionIDMeta on a malformed frame = %q, want empty", got)
	}
}
