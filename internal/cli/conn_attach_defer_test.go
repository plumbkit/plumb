package cli

// conn_attach_defer_test.go — PLAN-395: the pre-Execute pin in onBeforeTool
// runs BEFORE session_start's Execute commits the caller's identity, so for a
// call that DECLARES one it can neither know the caller is one of several
// agents nor route the pin to that agent's shard. It stamped the connection
// sticky (PinSourceSessionStart) on behalf of an agent it could not yet see —
// the last place #404's "identity before workspace" ordering did not reach.
// The fix: a declaring call defers the pin to Execute entirely; an undeclared
// call keeps the pre-pin byte for byte.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

func deferTestSession(t *testing.T) *connSession {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	return s
}

// TestOnBeforeTool_DeclaredSessionIDDoesNotPinTheConnection is the defect,
// deterministic: a session_start whose arguments declare a session_id must not
// move the CONNECTION during onBeforeTool — at that point the identity is not
// even committed, so the pin is attributed to nobody and is sticky besides.
func TestOnBeforeTool_DeclaredSessionIDDoesNotPinTheConnection(t *testing.T) {
	s := deferTestSession(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s.onBeforeTool(context.Background(), "session_start", []byte(`{"workspace":"`+root+`","session_id":"sub-1"}`))

	if got := s.workspace(); got != "" {
		t.Fatalf("the pre-Execute pin moved the CONNECTION to %q for a call that declares session_id — "+
			"it pinned, sticky, on behalf of an identity that has not been committed; Execute must own this pin (PLAN-395)", got)
	}
}

// TestOnBeforeTool_PerCallIdentityDoesNotPinTheConnection: the same rule for
// the per-call _meta channel, which — unlike session_id — can arrive on ANY
// tool's arguments.
func TestOnBeforeTool_PerCallIdentityDoesNotPinTheConnection(t *testing.T) {
	s := deferTestSession(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s.onBeforeTool(mcp.WithLogicalAgent(context.Background(), "agent-a"), "session_start", []byte(`{"workspace":"`+root+`"}`))

	if got := s.workspace(); got != "" {
		t.Fatalf("the pre-Execute pin moved the CONNECTION to %q for a call carrying a per-call identity (PLAN-395)", got)
	}
}

// TestOnBeforeTool_UndeclaredWorkspaceArgStillPins guards the path the fix
// must not disturb: an UNDECLARED call keeps the pre-pin, because the attach
// ladder for clients that never declare depends on it and the fail-closed
// first-contact refusal for undeclared agents rides it.
func TestOnBeforeTool_UndeclaredWorkspaceArgStillPins(t *testing.T) {
	s := deferTestSession(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s.onBeforeTool(context.Background(), "session_start", []byte(`{"workspace":"`+root+`"}`))

	if got := s.workspace(); got != root {
		t.Fatalf("an undeclared workspace-arg pre-pin landed on %q, want %q — the undeclared attach path must not change (PLAN-395)", got, root)
	}
}

// TestOnBeforeTool_DeclaredCallStillPinsThroughExecute guards against
// under-deferring: a lone declared agent's session_start must still end with
// the connection pinned, through Execute's identity-then-workspace ordering
// (identity commits first, so the repin resolves to the connection level —
// one known agent IS the connection).
func TestOnBeforeTool_DeclaredCallStillPinsThroughExecute(t *testing.T) {
	s := deferTestSession(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	start := tools.NewSessionStart(s.workspaceFor, nil, nil, nil, func() string { return "" }, nil).
		WithRepin(s.repinWorkspace).
		WithDeclaredAgent(s.declaredAgentCtx).
		WithExternalID(func(id string) string {
			s.recordLogicalAgentAttach(id)
			return ""
		})
	raw, err := json.Marshal(map[string]any{"workspace": root, "session_id": "solo"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	ctx := context.Background()
	if err := s.refuseSharedStateChange(ctx, "session_start", ""); err != nil {
		t.Fatalf("session_start refused up front: %v", err)
	}
	s.onBeforeTool(ctx, "session_start", raw)
	if _, err := start.Execute(ctx, raw); err != nil {
		t.Fatalf("session_start: %v", err)
	}

	if got := s.workspace(); got != root {
		t.Fatalf("a lone declared agent's session_start must still pin the connection through Execute, got %q (PLAN-395 deferred too much)", got)
	}
}
