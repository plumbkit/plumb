package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// TestLogicalAgentRidesCtx pins the transport half of per-agent keying: the
// logical-agent identity a tools/call declares must be readable from the ctx the
// tool and its hooks receive, so a concurrent multiplexed call cannot read a
// peer's agent from a mutable connection field.
func TestLogicalAgentRidesCtx(t *testing.T) {
	s := newServer()
	var got string
	s.OnBeforeTool = func(ctx context.Context, name string, args json.RawMessage, logicalAgent string) {
		got = mcp.LogicalAgentFromCtx(ctx)
	}

	withID := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"},"_meta":{"%s":"agent-7"}}}`, mcp.MetaLogicalAgentKey)
	serveOn(t, s, withID)
	if got != "agent-7" {
		t.Fatalf("LogicalAgentFromCtx(ctx) = %q, want agent-7", got)
	}

	// A call with no identity must leave the ctx empty, not leak a prior value.
	got = ""
	serveOn(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	if got != "" {
		t.Fatalf("LogicalAgentFromCtx(ctx) = %q on an anonymous call, want empty", got)
	}
}
