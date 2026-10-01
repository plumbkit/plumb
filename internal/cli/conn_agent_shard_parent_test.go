package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

func newThreeRootConn(t *testing.T) (s *connSession, rootMain, worktree, other string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s = newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	rootMain, worktree, other = freshTempDir(t), freshTempDir(t), freshTempDir(t)
	for _, d := range []string{rootMain, worktree, other} {
		mustGitDir(t, d)
	}
	newSharedConn(t, s, rootMain, "conv-x", "conv-y")
	return s, rootMain, worktree, other
}

// Round-2 review of #535, N1: a subagent whose shard already exists when its
// conversation re-pins itself (a background subagent, a continued one) must
// follow — seeding only at creation left it in the other agent's checkout.
// A subagent that chose a root of its own stays where it chose.
func TestSubagentFollowsItsConversationsLaterRepin(t *testing.T) {
	s, rootMain, worktree, other := newThreeRootConn(t)
	if got := s.workspaceFor(idCtx("conv-y/sub")); got != rootMain {
		t.Fatalf("precondition: the subagent starts on the connection root, got %q", got)
	}
	// Another conversation's subagent, on the same root BEFORE the move.
	_ = s.workspaceFor(idCtx("conv-x/sub"))
	if _, err := s.repinWorkspace(idCtx("conv-y/own"), "file://"+other, "", true, false); err != nil {
		t.Fatalf("subagent's own re-pin: %v", err)
	}
	// A subagent that explicitly chose the root it was seeded on: no root
	// moves, but it is now its own choice, so it must not follow either.
	if _, err := s.repinWorkspace(idCtx("conv-y/here"), "file://"+rootMain, "", false, false); err != nil {
		t.Fatalf("subagent's same-root declaration: %v", err)
	}
	if _, err := s.repinWorkspace(idCtx("conv-y"), "file://"+worktree, "", true, false); err != nil {
		t.Fatalf("conv-y re-pin: %v", err)
	}
	if got := s.workspaceFor(idCtx("conv-y/here")); got != rootMain {
		t.Errorf("a subagent that declared %q followed its conversation to %q", rootMain, got)
	}
	if got := s.workspaceFor(idCtx("conv-y/sub")); got != worktree {
		t.Errorf("a subagent created before its conversation's re-pin resolves to %q, want %q", got, worktree)
	}
	if got := s.workspaceFor(idCtx("conv-y/own")); got != other {
		t.Errorf("a subagent that chose %q was moved to %q", other, got)
	}
	if got := s.workspaceFor(idCtx("conv-x/sub")); got != rootMain {
		t.Errorf("another conversation's subagent moved to %q", got)
	}
	// And it keeps following: the conversation moves again.
	if _, err := s.repinWorkspace(idCtx("conv-y"), "file://"+rootMain, "", true, false); err != nil {
		t.Fatalf("conv-y second re-pin: %v", err)
	}
	if got := s.workspaceFor(idCtx("conv-y/sub")); got != rootMain {
		t.Errorf("after the second re-pin the subagent resolves to %q, want %q", got, rootMain)
	}
}

// N1b: a connection-scope move must not drag a subagent sitting on its
// conversation's CHOSEN root, even when that root equals the connection's.
func TestConnectionMoveDoesNotDragAParentSeededSubagent(t *testing.T) {
	s, rootMain, worktree, other := newThreeRootConn(t)
	// conv-y chooses rootMain (away and back), so it is self-pinned there.
	for _, r := range []string{worktree, rootMain} {
		if _, err := s.repinWorkspace(idCtx("conv-y"), "file://"+r, "", true, false); err != nil {
			t.Fatalf("conv-y re-pin to %s: %v", r, err)
		}
	}
	_ = s.workspaceFor(idCtx("conv-y/sub"))  // created from the parent's chosen root
	_ = s.workspaceFor(idCtx("conv-x/seed")) // control: seeded from the connection
	// A subagent created while ITS conversation sat on the seed; the
	// conversation then declares that same root (no move, so nothing re-seeds
	// the subagent) and so chose it.
	_ = s.workspaceFor(idCtx("conv-z/early"))
	s.recordLogicalAgentAttach("conv-z")
	if _, err := s.repinWorkspace(idCtx("conv-z"), "file://"+rootMain, "", false, false); err != nil {
		t.Fatalf("conv-z same-root declaration: %v", err)
	}
	if _, err := s.repinWorkspace(idCtx("conv-x"), "file://"+other, "", true, true); err != nil {
		t.Fatalf("connection move: %v", err)
	}
	if got := s.workspace(); got != other {
		t.Fatalf("precondition: the connection moved to %q, want %q", got, other)
	}
	if got := s.workspaceFor(idCtx("conv-y/sub")); got != rootMain {
		t.Errorf("a connection move dragged conv-y's subagent to %q; its conversation is on %q", got, rootMain)
	}
	if got := s.workspaceFor(idCtx("conv-z/early")); got != rootMain {
		t.Errorf("a connection move dragged conv-z's subagent to %q; its conversation chose %q", got, rootMain)
	}
	if got := s.workspaceFor(idCtx("conv-x/seed")); got != other {
		t.Errorf("control: a connection-seeded subagent must follow the connection, got %q", got)
	}
}

// The same after a restart: the parent's shard is not in memory, its persisted
// pin seeded the subagent, and a connection move must not drag it.
func TestConnectionMoveDoesNotDragASubagentSeededFromAPersistedParent(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-513-parent-pin-move"
	rootMain, other := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootMain)
	mustGitDir(t, other)
	if err := ss.UpsertPinForAgent(proxyID, "conv-y", rootMain, "", sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("persist parent pin: %v", err)
	}
	s := newPersistSession(t, store, ss, proxyID)
	newSharedConn(t, s, rootMain, "conv-x", "conv-y")
	_ = s.workspaceFor(idCtx("conv-y/sub"))
	if _, err := s.repinWorkspace(idCtx("conv-x"), "file://"+other, "", true, true); err != nil {
		t.Fatalf("connection move: %v", err)
	}
	if got := s.workspaceFor(idCtx("conv-y/sub")); got != rootMain {
		t.Errorf("a connection move dragged a subagent seeded from its conversation's persisted pin to %q", got)
	}
}

// A -race and deadlock guard for the follow paths: a conversation re-pins back
// and forth while its subagents make first calls and peers walk the shards.
func TestConcurrentParentRepinAndSubagentFirstCalls(t *testing.T) {
	s, rootMain, worktree, _ := newThreeRootConn(t)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := range 40 {
			r := worktree
			if i%2 == 1 {
				r = rootMain
			}
			_, _ = s.repinWorkspace(idCtx("conv-y"), "file://"+r, "", true, false)
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 400 {
			ctx := idCtx(fmt.Sprintf("conv-y/sub-%d", i))
			_ = s.workspaceFor(ctx)
			_ = s.declarationRefusedErr(ctx)
			_ = s.checkBoundaryFor(ctx, "x.go", 0)
		}
	}()
	go func() {
		defer wg.Done()
		for range 400 {
			_ = s.pinnedPolicyGuard("/tmp/x")
			_ = s.workspaceFor(idCtx("conv-x"))
		}
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("deadlock: concurrent parent re-pins and subagent first calls did not finish")
	}
	// Settled: the conversation ended on rootMain (an even count of moves), and
	// every subagent that never chose a root sits with it.
	want := s.workspaceFor(idCtx("conv-y"))
	for _, i := range []int{0, 199, 399} {
		if got := s.workspaceFor(idCtx(fmt.Sprintf("conv-y/sub-%d", i))); got != want {
			t.Errorf("conv-y/sub-%d resolves to %q, its conversation to %q", i, got, want)
		}
	}
}

// A subagent whose root was restored from its OWN persisted pin chose it, and
// does not follow its conversation's later move.
func TestRestoredSubagentDoesNotFollowItsConversation(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-513-restored-sub"
	rootMain, worktree := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootMain)
	mustGitDir(t, worktree)
	if err := ss.UpsertPinForAgent(proxyID, "conv-y/r", rootMain, "", sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("persist subagent pin: %v", err)
	}
	s := newPersistSession(t, store, ss, proxyID)
	newSharedConn(t, s, rootMain, "conv-x", "conv-y")
	_ = s.workspaceFor(idCtx("conv-y/r"))
	_ = s.workspaceFor(idCtx("conv-y/s")) // control: seeded, follows
	if _, err := s.repinWorkspace(idCtx("conv-y"), "file://"+worktree, "", true, false); err != nil {
		t.Fatalf("conv-y re-pin: %v", err)
	}
	if got := s.workspaceFor(idCtx("conv-y/r")); got != rootMain {
		t.Errorf("a subagent restored onto %q followed its conversation to %q", rootMain, got)
	}
	if got := s.workspaceFor(idCtx("conv-y/s")); got != worktree {
		t.Errorf("control: a seeded subagent must follow, got %q", got)
	}
}
