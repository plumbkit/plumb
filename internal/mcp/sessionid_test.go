package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

func TestOnSessionIDFiresOnInitialize(t *testing.T) {
	s := newServer()
	var got string
	s.OnSessionID = func(_ context.Context, id string) { got = id }
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"_meta": map[string]string{mcp.MetaSessionIDKey: "sess-123"},
		},
	})
	serveOn(t, s, string(req))
	if got != "sess-123" {
		t.Fatalf("OnSessionID got %q, want sess-123", got)
	}
}

func TestOnSessionIDNotFiredWhenAbsent(t *testing.T) {
	s := newServer()
	fired := false
	s.OnSessionID = func(_ context.Context, id string) { fired = true }
	serveOn(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if fired {
		t.Fatal("OnSessionID must not fire when the key is absent")
	}
}
