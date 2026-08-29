package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// declaredAgentKeyType is a test-local ctx key standing in for the daemon's
// logical-agent key (internal/mcp owns the real one, and the layering contract
// keeps this package from importing it).
type declaredAgentKeyType struct{}

// TestSessionStart_AttributionPrecedesTheWorkspace pins the ORDER session_start
// does its work in. A subagent multiplexed over a shared connection declares its
// identity with `session_id` in the same call that names its workspace; if the
// workspace re-pin runs before that identity reaches the ctx, the call is
// unattributable exactly when the daemon decides whose pin to move, and the
// re-pin lands on the connection — dragging every peer agent's workspace with it
// (issue #182).
//
// The linkage half runs the other way round: it COMMITS things (see
// TestSessionStart_LinkageNotCommittedOnARefusedCall), so it must come after.
func TestSessionStart_AttributionPrecedesTheWorkspace(t *testing.T) {
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
	want := []string{"declared-agent", "repin", "external-id"}
	if len(order) != len(want) {
		t.Fatalf("call order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("call order = %v, want %v", order, want)
		}
	}
}

// TestSessionStart_LinkageNotCommittedOnARefusedCall guards the other half of
// that split. The external-ID linker does not observe, it COMMITS: it makes the
// session answerable to this id from plumb mail and the wake hook, it may rename
// the session to inherit an ended one's name, and on the daemon side it records
// the attach-time fallback identity that every unattributed call is then
// resolved against. A call the daemon REFUSED must commit none of it — an agent
// whose re-pin was refused never attached, and a session that answers to its id
// is a lie about what happened.
//
// The attribution channel still fires: the refusal itself has to be attributed
// to the agent that asked, or it is the connection's pin being refused, not
// theirs.
func TestSessionStart_LinkageNotCommittedOnARefusedCall(t *testing.T) {
	ws := t.TempDir()
	linked, attributed := false, false
	tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
		WithExternalID(func(string) string { linked = true; return "" }).
		WithDeclaredAgent(func(ctx context.Context, id string) context.Context {
			attributed = true
			return context.WithValue(ctx, declaredAgentKeyType{}, id)
		}).
		WithRepin(func(context.Context, string, string, bool) (string, error) {
			return "", errors.New("refusing to re-pin: sticky (issue #182)")
		})

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+t.TempDir()+`","session_id":"drifter"}`))
	if err == nil {
		t.Fatal("precondition: the re-pin was supposed to be refused")
	}
	if !attributed {
		t.Error("the refused call was never attributed to the agent that made it")
	}
	if linked {
		t.Error("a REFUSED session_start committed the external-ID linkage: the session now answers to an id whose call was rejected, may have inherited an ended session's name, and the attach-time fallback identity points at an agent that never attached")
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
