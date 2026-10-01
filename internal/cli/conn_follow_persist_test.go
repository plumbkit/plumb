package cli

// conn_follow_persist_test.go — a pin a shard was merely DRAGGED to is not a pin
// its agent chose (issue #527, first half).
//
// followConnectionShards used to persist the connection's new root as the
// dragged shard's own per-agent row. shardFor reads that row back as a root the
// agent DECLARED — the rule both it and attributeConnectionPin state is that only
// an agent's own move or confirm writes one — so after a restart the row
// outranked the connection's current pin and the agent was treated as sticky at
// a place it never chose.
//
// The fixture moves the connection through client roots, the realistic way a
// follower is dragged without anyone naming a workspace, and brings the
// connection back from a restart at a DIFFERENT root: that is where a stale row
// and the connection disagree, which is the only place the defect is visible.

import (
	"context"
	"log/slog"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// followedThenRestarted drags a subagent's shard from rootX to rootZ with a roots
// notification, restarts the daemon, and brings the connection back at rootQ.
func followedThenRestarted(t *testing.T, proxyID string) (second *connSession, ctxSub context.Context, rootZ, rootQ string) {
	t.Helper()
	store, ss := newOriginStore(t)
	rootX, rootQ := freshTempDir(t), freshTempDir(t)
	rootZ = freshTempDir(t)
	for _, r := range []string{rootX, rootZ, rootQ} {
		mustGitDir(t, r)
	}
	ctxSub = mcp.WithLogicalAgent(context.Background(), "sub")

	first := newPersistSession(t, store, ss, proxyID)
	first.attachWorkspace(context.Background(), "file://"+rootX)
	first.recordLogicalAgentAttach("coordinator")
	first.recordLogicalAgentAttach("sub")
	// The subagent calls once, so its shard is seeded from the connection at X.
	if got := first.workspaceFor(ctxSub); got != rootX {
		t.Fatalf("precondition: the shard seeded at %q, want %q", got, rootX)
	}
	// The client moves its folder: the connection goes to Z and the shard follows.
	first.onRootsChanged(context.Background(), []string{"file://" + rootZ})
	if got := first.workspaceFor(ctxSub); got != rootZ {
		t.Fatalf("precondition: the shard did not follow the connection to %q (at %q)", rootZ, got)
	}
	if _, _, _, ok, err := ss.LoadPinForAgent(proxyID, "sub"); err != nil {
		t.Fatalf("LoadPinForAgent: %v", err)
	} else if ok {
		t.Error("a shard that only FOLLOWED the connection was persisted as its agent's own pin")
	}
	first.close()

	// The restart. The client now reports rootQ, and the connection's own pin was
	// set from roots, so it does not outrank them.
	calls := 0
	second = newPersistSession(t, store, ss, proxyID)
	second.attachOnInit(context.Background(), rootsReplying(rootQ, &calls))
	second.recordLogicalAgentAttach("coordinator")
	second.recordLogicalAgentAttach("sub")
	if got := second.workspace(); got != rootQ {
		t.Fatalf("precondition: the connection came back at %q, want the client's root %q", got, rootQ)
	}
	return second, ctxSub, rootZ, rootQ
}

// TestADraggedShardIsNotRestoredAsAChosenPin: the agent never chose Z, so after
// the restart it sits where the connection sits.
func TestADraggedShardIsNotRestoredAsAChosenPin(t *testing.T) {
	second, ctxSub, rootZ, rootQ := followedThenRestarted(t, "proxy-dragged")

	if got := second.workspaceFor(ctxSub); got != rootQ {
		t.Fatalf("after the restart the dragged agent resolves to %q, want the connection's %q — it was restored sticky at %q, a place it never chose",
			got, rootQ, rootZ)
	}
}

// TestADraggedShardStillFollowsAfterARestart is the consequence the issue names:
// a later connection move must take the agent along, as it would have before the
// restart.
func TestADraggedShardStillFollowsAfterARestart(t *testing.T) {
	second, ctxSub, _, _ := followedThenRestarted(t, "proxy-dragged-follows")
	rootY := freshTempDir(t)
	mustGitDir(t, rootY)

	_ = second.workspaceFor(ctxSub) // build the shard, as the agent's next call does
	second.onRootsChanged(context.Background(), []string{"file://" + rootY})

	if got := second.workspace(); got != rootY {
		t.Fatalf("precondition: the connection moved to %q, not %q", got, rootY)
	}
	if got := second.workspaceFor(ctxSub); got != rootY {
		t.Errorf("the agent resolves to %q after the connection moved to %q: it was pinned in place by a row it never wrote", got, rootY)
	}
}

// TestAChosenPinIsStillPersistedAndStillRestored is the positive control for the
// two tests above, in the direction the fix could break: an agent's OWN pin is
// still written, still survives a connection move, and still comes back.
func TestAChosenPinIsStillPersistedAndStillRestored(t *testing.T) {
	store, ss := newOriginStore(t)
	rootX, rootW, rootZ := freshTempDir(t), freshTempDir(t), freshTempDir(t)
	for _, r := range []string{rootX, rootW, rootZ} {
		mustGitDir(t, r)
	}
	ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")

	first := newPersistSession(t, store, ss, "proxy-chosen-row")
	first.attachWorkspace(context.Background(), "file://"+rootX)
	first.recordLogicalAgentAttach("coordinator")
	first.recordLogicalAgentAttach("sub")
	if _, err := first.repinWorkspace(ctxSub, rootW, "", true, false); err != nil {
		t.Fatalf("setup: the agent's own pin: %v", err)
	}
	first.onRootsChanged(context.Background(), []string{"file://" + rootZ})
	if got := first.workspaceFor(ctxSub); got != rootW {
		t.Fatalf("the connection's move dragged a self-pinned shard to %q, want %q", got, rootW)
	}
	if root, _, _, ok, err := ss.LoadPinForAgent("proxy-chosen-row", "sub"); err != nil || !ok || root != rootW {
		t.Fatalf("the agent's own pin row = %q ok=%v err=%v, want %q to survive the connection's move", root, ok, err, rootW)
	}
	first.close()

	second := newPersistSession(t, store, ss, "proxy-chosen-row")
	second.attachWorkspace(context.Background(), "file://"+rootX)
	second.recordLogicalAgentAttach("coordinator")
	second.recordLogicalAgentAttach("sub")
	if got := second.workspaceFor(ctxSub); got != rootW {
		t.Errorf("the agent's own pin came back as %q, want %q", got, rootW)
	}
}

// TestARestoredShardDoesNotFollowAConnectionMove: a row now means the agent
// CHOSE its root, so a restored shard is held to that however it came back. The
// connection happens to sit on the agent's root here, which is what lets a move
// off it drag a shard that sits on the previous root — and what let a restored
// shard be dragged off a workspace it had named, as a live self-pinned one never
// is (issue #468).
func TestARestoredShardDoesNotFollowAConnectionMove(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)
	elsewhere := freshTempDir(t)
	mustGitDir(t, elsewhere)
	ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")

	first := newPersistSession(t, store, ss, "proxy-restored-holds")
	first.attachWorkspace(context.Background(), "file://"+parent)
	first.recordLogicalAgentAttach("coordinator")
	first.recordLogicalAgentAttach("sub")
	if _, err := first.repinWorkspace(ctxSub, worktree, "", false, false); err != nil {
		t.Fatalf("setup: the agent's own pin: %v", err)
	}
	first.close()

	calls := 0
	second := newPersistSession(t, store, ss, "proxy-restored-holds")
	second.attachOnInit(context.Background(), rootsReplying(worktree, &calls))
	second.recordLogicalAgentAttach("coordinator")
	second.recordLogicalAgentAttach("sub")
	if got := second.workspaceFor(ctxSub); got != worktree {
		t.Fatalf("precondition: the restored shard sits at %q, want %q", got, worktree)
	}

	second.onRootsChanged(context.Background(), []string{"file://" + elsewhere})
	if got := second.workspace(); got != elsewhere {
		t.Fatalf("precondition: the connection moved to %q, not %q", got, elsewhere)
	}
	if got := second.workspaceFor(ctxSub); got != worktree {
		t.Errorf("a restored shard was dragged to %q by the connection's move; the agent named %q", got, worktree)
	}
}

// recordProbe is a slog handler that calls fn with each record's message, so a
// test can look at the world from inside the code that logs.
type recordProbe struct{ fn func(msg string) }

func (recordProbe) Enabled(context.Context, slog.Level) bool { return true }
func (p recordProbe) Handle(_ context.Context, r slog.Record) error {
	p.fn(r.Message)
	return nil
}
func (p recordProbe) WithAttrs([]slog.Attr) slog.Handler { return p }
func (p recordProbe) WithGroup(string) slog.Handler      { return p }

// TestAFollowedShardsRowIsForgottenWhileItsLockIsHeld: forgetting the row of a
// shard the connection dragged along must happen before the shard's lock is
// released. repinAgent writes the agent's row under that same lock, so a delete
// made after the release can land after a re-pin that ran in the gap, and wipe
// the row of a root the agent has just chosen.
//
// The probe sits inside the delete: the store is closed so the delete fails and
// is logged by forgetPinForAgent itself, and the logger tries to take the
// shard's lock at that moment. Asserting the lock is held after the call would
// pass under either order.
func TestAFollowedShardsRowIsForgottenWhileItsLockIsHeld(t *testing.T) {
	store, ss := newOriginStore(t)
	rootX, rootZ := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootX)
	mustGitDir(t, rootZ)
	ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")

	s := newPersistSession(t, store, ss, "proxy-forget-locked")
	s.attachWorkspace(context.Background(), "file://"+rootX)
	s.recordLogicalAgentAttach("coordinator")
	s.recordLogicalAgentAttach("sub")
	sh := s.shardFor(ctxSub)
	if sh == nil {
		t.Fatal("precondition: the subagent has no shard on a shared connection")
	}

	ss.Close() // every store call from here on fails, and forgetPinForAgent logs it
	var forgot, locked bool
	s.logger = slog.New(recordProbe{fn: func(msg string) {
		if msg != "daemon: forget agent pin failed" {
			return
		}
		forgot = true
		if sh.mu.TryLock() {
			sh.mu.Unlock()
			return
		}
		locked = true
	}})

	s.onRootsChanged(context.Background(), []string{"file://" + rootZ})

	if got := s.workspaceFor(ctxSub); got != rootZ {
		t.Fatalf("precondition: the shard did not follow the connection to %q (at %q)", rootZ, got)
	}
	if !forgot {
		t.Fatal("the dragged shard's row was never forgotten, so the probe never ran")
	}
	if !locked {
		t.Error("the shard's row was forgotten after its lock was released: a re-pin in the gap writes a row this delete then wipes")
	}
}
