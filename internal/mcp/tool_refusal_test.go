package mcp_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// TestToolRefusalHook proves the OnToolRefusal seam refuses a call before the
// tool runs and delivers the refusal as an isError result, and that a call
// declaring a logical-agent identity reaches the hook with that ID (so a
// multiplexed connection can attribute and thus NOT refuse it).
func TestToolRefusalHook(t *testing.T) {
	s := newServer()
	s.OnToolRefusal = func(_ context.Context, name, logicalAgent string) error {
		if name == "echo" && logicalAgent == "" {
			return errors.New("refused: anonymous echo on a shared connection")
		}
		return nil
	}

	// No identity: refused, and the tool must not have run.
	resps := serveOn(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("anonymous call must be refused with isError=true, got %v", result["isError"])
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "refused: anonymous echo") {
		t.Fatalf("refusal message missing, got %q", text)
	}

	// With a per-call logical-agent identity: attributed, so the hook lets it run.
	resps = serveOn(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"},"_meta":{"`+mcp.MetaLogicalAgentKey+`":"agent-1"}}}`)
	result = resps[0]["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("attributed call must not be refused, got %v", result["content"])
	}
	content = result["content"].([]any)
	text = content[0].(map[string]any)["text"].(string)
	if text != "hi" {
		t.Fatalf("attributed echo must return its text, got %q", text)
	}
}
