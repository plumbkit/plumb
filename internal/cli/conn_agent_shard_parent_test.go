package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

func idCtx(id string) context.Context {
	return mcp.WithLogicalAgent(context.Background(), id)
}

// newSharedConn builds a connection pinned to rootMain with the given agents
// declared at attach, so it is shared.
func newSharedConn(t *testing.T, s *connSession, rootMain string, agents ...string) {
	t.Helper()
	s.attachWorkspace(context.Background(), "file://"+rootMain)
	if got := s.workspace(); got != rootMain {
		t.Fatalf("precondition: connection pinned to %q, want %q", got, rootMain)
	}
	for _, a := range agents {
		s.recordLogicalAgentAttach(a)
	}
}

// Issue #513 review B1: a hook-stamped subagent is admitted on its
// conversation's declaration, so it must work where its conversation chose to.
// Seeded from the connection pin instead, a subagent of a parent that had
// re-pinned itself to a worktree wrote into the other agent's checkout.
func TestSubagentInheritsItsConversationsChosenRoot(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	rootMain, worktree := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootMain)
	mustGitDir(t, worktree)
	newSharedConn(t, s, rootMain, "conv-x", "conv-y")

	if _, err := s.repinWorkspace(idCtx("conv-y"), "file://"+worktree, "", true, false); err != nil {
		t.Fatalf("conv-y re-pin: %v", err)
	}
	if got := s.workspaceFor(idCtx("conv-y/sub")); got != worktree {
		t.Errorf("conv-y's subagent resolves to %q, want its conversation's worktree %q", got, worktree)
	}
	// Control: a subagent whose conversation never chose a root sits where the
	// connection (and so its conversation) sits.
	if got := s.workspaceFor(idCtx("conv-x/sub")); got != rootMain {
		t.Errorf("conv-x's subagent resolves to %q, want the connection's %q", got, rootMain)
	}
}

// After a daemon restart the parent's shard is not in memory until its next
// call; its persisted per-agent pin is the evidence of where it chose to work.
func TestSubagentInheritsItsConversationsPersistedRoot(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-513-parent-pin"
	rootMain, worktree := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootMain)
	mustGitDir(t, worktree)
	if err := ss.UpsertPinForAgent(proxyID, "conv-y", worktree, "", sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("persist parent pin: %v", err)
	}
	s := newPersistSession(t, store, ss, proxyID)
	newSharedConn(t, s, rootMain, "conv-x", "conv-y")

	s.shardsMu.Lock()
	_, parentLive := s.shards["conv-y"]
	s.shardsMu.Unlock()
	if parentLive {
		t.Fatal("precondition: the parent's shard must not be in memory yet")
	}
	if got := s.workspaceFor(idCtx("conv-y/sub")); got != worktree {
		t.Errorf("after a restart conv-y's subagent resolves to %q, want its conversation's persisted %q", got, worktree)
	}
	if got := s.workspaceFor(idCtx("conv-x/sub")); got != rootMain {
		t.Errorf("control: conv-x's subagent resolves to %q, want the connection's %q", got, rootMain)
	}
}

// A subagent of a conversation whose declaration was REFUSED sits on the very
// root its conversation was refused off; it inherits the refusal until it
// chooses a root of its own.
func TestSubagentInheritsItsConversationsRefusedDeclaration(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	rootMain, requested, own := freshTempDir(t), freshTempDir(t), freshTempDir(t)
	for _, d := range []string{rootMain, requested, own} {
		mustGitDir(t, d)
	}
	newSharedConn(t, s, rootMain, "conv-p", "conv-q")
	s.markDeclarationRefused("conv-p", requested, rootMain)

	if got := s.workspaceFor(idCtx("conv-p/sub")); got != "" {
		t.Errorf("a subagent of a refused conversation resolves to %q; it must resolve nowhere", got)
	}
	err := s.declarationRefusedErr(idCtx("conv-p/sub"))
	if err == nil || !strings.Contains(err.Error(), `conversation "conv-p"`) {
		t.Errorf("a subagent of a refused conversation must be refused naming it, got %v", err)
	}
	// Controls: another conversation's subagent is untouched.
	if got := s.workspaceFor(idCtx("conv-q/sub")); got != rootMain {
		t.Errorf("conv-q's subagent resolves to %q, want %q", got, rootMain)
	}
	if err := s.declarationRefusedErr(idCtx("conv-q/sub")); err != nil {
		t.Errorf("conv-q's subagent refused: %v", err)
	}
	// A subagent that chose its own root is no longer held by its parent.
	if _, err := s.repinWorkspace(idCtx("conv-p/sub"), "file://"+own, "", true, false); err != nil {
		t.Fatalf("subagent re-pin: %v", err)
	}
	if got := s.workspaceFor(idCtx("conv-p/sub")); got != own {
		t.Errorf("a subagent that chose %q resolves to %q", own, got)
	}
	if err := s.declarationRefusedErr(idCtx("conv-p/sub")); err != nil {
		t.Errorf("a subagent that chose its own root is still refused: %v", err)
	}
}
