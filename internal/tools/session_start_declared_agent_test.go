package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// declaredAgentKeyType is a test-local ctx key standing in for the daemon's
// logical-agent key (internal/mcp owns the real one, and the layering contract
// keeps this package from importing it).
type declaredAgentKeyType struct{}

// TestSessionStart_IdentityIsResolvedBeforeTheWorkspace pins the ORDER
// session_start does its work in. A subagent multiplexed over a shared
// connection declares its identity with `session_id` in the same call that names
// its workspace; if the workspace re-pin runs first, that call is unattributable
// exactly when the daemon decides whose pin to move, and the re-pin lands on the
// connection — dragging every peer agent's workspace with it (issue #182).
//
// Both identity channels must therefore be settled before the re-pin: the
// external-ID linker AND the ctx the re-pin runs under.
func TestSessionStart_IdentityIsResolvedBeforeTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	var order []string
	tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
		WithExternalID(func(string) string { order = append(order, "external-id"); return "" }).
		WithDeclaredAgent(func(ctx context.Context, id string) context.Context {
			order = append(order, "declared-agent")
			return context.WithValue(ctx, declaredAgentKeyType{}, id)
		}).
		WithRepin(func(ctx context.Context, workspace, _ string, _ bool) (string, error) {
			order = append(order, "repin")
			if got, _ := ctx.Value(declaredAgentKeyType{}).(string); got != "subagent-7" {
				t.Errorf("re-pin ctx logical agent = %q, want %q — the re-pin ran unattributed", got, "subagent-7")
			}
			return workspace, nil
		})

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+ws+`","session_id":"subagent-7"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []string{"external-id", "declared-agent", "repin"}
	if len(order) != len(want) {
		t.Fatalf("call order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("call order = %v, want %v", order, want)
		}
	}
}

// TestSessionStart_DeclaredAgentSkippedWithoutSessionID keeps the single-agent
// path untouched: with no session_id there is nothing to attribute, so the ctx
// channel must not fire at all (a synthesised identity would mark an ordinary
// connection shared and switch on per-agent keying nobody asked for).
func TestSessionStart_DeclaredAgentSkippedWithoutSessionID(t *testing.T) {
	ws := t.TempDir()
	called := false
	tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
		WithDeclaredAgent(func(ctx context.Context, _ string) context.Context { called = true; return ctx })

	for _, raw := range []string{`{}`, `{"session_id":""}`, `{"workspace":"` + ws + `"}`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(raw)); err != nil {
			t.Fatalf("Execute(%s): %v", raw, err)
		}
		if called {
			t.Fatalf("the declared-agent channel fired for %s, which declares no identity", raw)
		}
	}
}
