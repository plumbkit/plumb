package cli

// conn_restored_first_caller_test.go — the first agent to call after a restart
// (issue #523).
//
// The set of identities a connection has seen lives in memory, so a restart
// empties it, and the first stamped call is, to the connection, a lone agent:
// sharedWith counts it as the only identity, shardFor declines, and the call ran
// on the CONNECTION's pin and read tracker. In the house pattern the parent is
// parked on the Agent tool while a subagent works, so the subagent is routinely
// that first caller. TestRestoredShardStaysStickyAcrossARestart cannot see it:
// both its agents re-declare before it asserts anything.
//
// What the fix must NOT do is as much the subject here as what it must. A lone
// stamped agent also has a per-agent row (an explicit session_start that landed
// on the connection is attributed to its caller, issue #468), so "this agent has
// a row" cannot on its own mean "give it a shard": that would take a
// single-agent connection's session_start off the connection for good, and the
// connection pin, the language server and the session record would stop
// following it. Only durable evidence that the connection WAS shared (two
// identities declared together) routes an agent to its restored shard.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// restartedSharedConn builds the pre-restart world and returns the connection
// that comes back after it: a coordinator and a subagent were multiplexed over
// one proxy session, the subagent chose its worktree and read a file there, and
// the daemon has since restarted. The coordinator is parked, so nothing has
// declared itself on the new connection yet.
func restartedSharedConn(t *testing.T, proxyID string) (second *connSession, parent, worktree, readPath string, readAt time.Time) {
	t.Helper()
	store, ss := newOriginStore(t)
	parent, worktree = worktreeUnderParent(t)
	ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")
	readPath = filepath.Join(worktree, "notes.md")
	readAt = time.Unix(1_700_000_000, 123)

	first := newPersistSession(t, store, ss, proxyID)
	first.attachWorkspace(context.Background(), "file://"+parent)
	first.recordLogicalAgentAttach("coordinator")
	first.recordLogicalAgentAttach("sub")
	if _, err := first.repinWorkspace(ctxSub, worktree, "", false, false); err != nil {
		t.Fatalf("setup: the subagent's own pin: %v", err)
	}
	first.readTrackerFor(ctxSub).Record(readPath, readAt, "sha-notes")
	first.close()

	second = newPersistSession(t, store, ss, proxyID)
	second.attachWorkspace(context.Background(), "file://"+parent)
	return second, parent, worktree, readPath, readAt
}

// TestFirstCallerAfterARestartRunsOnItsOwnRestoredShard: ONLY the subagent calls
// after the restart, and it must get its own restored pin and read tracker.
func TestFirstCallerAfterARestartRunsOnItsOwnRestoredShard(t *testing.T) {
	second, parent, worktree, readPath, readAt := restartedSharedConn(t, "proxy-first-caller")
	ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")

	// What the server's OnBeforeTool does for a stamped call: the connection now
	// knows ONE identity, which is exactly why sharedWith says "not shared".
	second.recordLogicalAgentCall("sub")

	if got := second.workspaceFor(ctxSub); got != worktree {
		t.Fatalf("the first caller resolves to %q, want its restored pin %q — a relative write would land in %q",
			got, worktree, filepath.Join(parent, "x"))
	}
	if second.readTrackerFor(ctxSub) == second.readTracker {
		t.Fatal("the first caller runs on the CONNECTION's read tracker, so the parent's reloaded reads satisfy its strict-mode checks")
	}
	if got := second.readTrackerFor(ctxSub).Mtime(readPath); !got.Equal(readAt) {
		t.Errorf("the restored shard's tracker holds mtime %v for %s, want its own persisted read %v", got, readPath, readAt)
	}
	if second.writeTrackerFor(ctxSub) == second.writeTracker {
		t.Error("the first caller shares the connection's write tracker")
	}
	if _, err := second.policyFor(ctxSub).Check(filepath.Join(worktree, "notes.md"), tools.AccessReadWrite); err != nil {
		t.Errorf("the restored shard's boundary refuses its own worktree: %v", err)
	}

	// A read it makes now is persisted under ITS id, where its shard looks later.
	later := filepath.Join(worktree, "later.md")
	second.readTrackerFor(ctxSub).Record(later, readAt, "sha-later")
	// Matched by name: the store keeps the canonical spelling of the path.
	recs, err := second.sessionState.LoadReadsForAgent("proxy-first-caller", "sub", worktree)
	if err != nil {
		t.Fatalf("LoadReadsForAgent: %v", err)
	}
	found := false
	for _, r := range recs {
		found = found || filepath.Base(r.Path) == "later.md"
	}
	if !found {
		t.Errorf("a read made by the restored agent was not persisted under its own id (rows: %+v)", recs)
	}
}

// TestRestoredRoutingDoesNotRearmTheAnonymousWriteGate: this is routing only.
// Re-arming the gate from durable evidence locked out a client with no per-call
// identity channel (see seedLogicalAgentsFromState), so the connection must still
// admit an anonymous write after the restart.
func TestRestoredRoutingDoesNotRearmTheAnonymousWriteGate(t *testing.T) {
	second, _, _, _, _ := restartedSharedConn(t, "proxy-first-caller-gate")
	ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")
	second.recordLogicalAgentCall("sub")

	_ = second.workspaceFor(ctxSub) // route the first caller to its restored shard
	if err := second.refuseSharedStateChange(context.Background(), "write_file", ""); err != nil {
		t.Errorf("restoring a shard re-armed the anonymous-write gate: %v", err)
	}
	if second.logicalAgents.sharedWith("") {
		t.Error("restoring a shard marked the connection shared; the observed-identity set must stay what the live calls made it")
	}
}

// TestALoneStampedAgentAfterARestartStaysOnTheConnection is the positive control
// for the narrowing above, run in the direction the fix could get wrong. A lone
// agent's explicit session_start leaves a per-agent row (attributeConnectionPin),
// and nothing else says the connection was ever shared. It must keep resolving
// against, and moving, the CONNECTION's pin.
func TestALoneStampedAgentAfterARestartStaysOnTheConnection(t *testing.T) {
	store, ss := newOriginStore(t)
	_, worktree := worktreeUnderParent(t)
	other := freshTempDir(t)
	mustGitDir(t, other)
	ctxSolo := mcp.WithLogicalAgent(context.Background(), "solo")

	first := newPersistSession(t, store, ss, "proxy-solo")
	if _, err := first.repinWorkspace(ctxSolo, worktree, "", false, false); err != nil {
		t.Fatalf("setup: the lone agent's session_start: %v", err)
	}
	first.close()

	second := newPersistSession(t, store, ss, "proxy-solo")
	second.attachOnInit(context.Background(), rootsSilent())
	second.recordLogicalAgentCall("solo")

	if sh := second.shardFor(ctxSolo); sh != nil {
		t.Fatal("a lone stamped agent was given a shard from a row that only attributes the connection's pin to it")
	}
	if got := second.workspaceFor(ctxSolo); got != worktree {
		t.Fatalf("the lone agent resolves to %q, want the restored connection pin %q", got, worktree)
	}
	if _, err := second.repinWorkspace(ctxSolo, other, "", true, false); err != nil {
		t.Fatalf("the lone agent's forced re-pin: %v", err)
	}
	if got := second.workspace(); got != other {
		t.Errorf("the lone agent's re-pin moved %q, not the connection pin (still %q): a restored row took it off the connection", other, got)
	}
}

// TestHistoricalDeclarationsDoNotRouteToARestoredShard: two identities declared
// long ago are not evidence of a shared connection now. Without the window a user
// who opens conversation after conversation over one long-lived serve would be
// routed onto a shard for every one of them.
func TestHistoricalDeclarationsDoNotRouteToARestoredShard(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)
	ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")

	first := newPersistSession(t, store, ss, "proxy-history")
	first.attachWorkspace(context.Background(), "file://"+parent)
	first.recordLogicalAgentAttach("coordinator")
	first.recordLogicalAgentAttach("sub")
	if _, err := first.repinWorkspace(ctxSub, worktree, "", false, false); err != nil {
		t.Fatalf("setup: the subagent's own pin: %v", err)
	}
	first.close()
	if err := ss.BackdateLogicalAgents("proxy-history", time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	second := newPersistSession(t, store, ss, "proxy-history")
	second.attachWorkspace(context.Background(), "file://"+parent)
	second.recordLogicalAgentCall("sub")

	if sh := second.shardFor(ctxSub); sh != nil {
		t.Error("declarations a month old routed a lone caller onto a shard; only recent, concurrent ones are evidence")
	}
}

// TestASubagentWithNoRowOfItsOwnIsAnchoredToItsParentAfterARestart is the common
// subagent. It never called session_start, so it has no pin row: it was anchored
// to its parent's chosen root, and that is where it resolved before the restart.
// With only the subagent calling afterwards, requiring a row of its own sent it
// to the connection's pin, which is the misroute #523 describes, one hop away
// from the case its row covers.
func TestASubagentWithNoRowOfItsOwnIsAnchoredToItsParentAfterARestart(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)
	ctxConv := mcp.WithLogicalAgent(context.Background(), "conv")
	ctxSub := mcp.WithLogicalAgent(context.Background(), "conv/sub")

	first := newPersistSession(t, store, ss, "proxy-anchored")
	first.attachWorkspace(context.Background(), "file://"+parent)
	first.recordLogicalAgentAttach("conv")
	first.recordLogicalAgentAttach("conv/sub")
	if _, err := first.repinWorkspace(ctxConv, worktree, "", false, false); err != nil {
		t.Fatalf("setup: the conversation's own pin: %v", err)
	}
	if got := first.workspaceFor(ctxSub); got != worktree {
		t.Fatalf("precondition: before the restart the subagent resolves to %q, want its parent's %q", got, worktree)
	}
	if _, _, _, ok, err := ss.LoadPinForAgent("proxy-anchored", "conv/sub"); err != nil || ok {
		t.Fatalf("precondition: the subagent has a row of its own (ok=%v err=%v); this test is about the one with none", ok, err)
	}
	first.close()

	second := newPersistSession(t, store, ss, "proxy-anchored")
	second.attachWorkspace(context.Background(), "file://"+parent)
	second.recordLogicalAgentCall("conv/sub") // only the subagent calls; its parent is parked

	if got := second.workspaceFor(ctxSub); got != worktree {
		t.Fatalf("after the restart the subagent resolves to %q, want its parent's %q — a relative write would land in %q",
			got, worktree, filepath.Join(parent, "x"))
	}
}

// TestASubagentAfterARestartDoesNotShareTheConnectionsReadTracker is the same gap
// seen through the read tracker. The parent, alone on the connection, read a file
// on the CONNECTION's tracker, which a restart reloads; the subagent that joined
// afterwards has no row, so it ran on that tracker and the parent's read record
// satisfied its strict-mode check for a file it never read.
func TestASubagentAfterARestartDoesNotShareTheConnectionsReadTracker(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, _ := worktreeUnderParent(t)
	notes := filepath.Join(parent, "notes.md")
	readAt := time.Unix(1_700_000_000, 321)
	ctxConv := mcp.WithLogicalAgent(context.Background(), "conv")
	ctxSub := mcp.WithLogicalAgent(context.Background(), "conv/sub")

	first := newPersistSession(t, store, ss, "proxy-shared-tracker")
	first.attachWorkspace(context.Background(), "file://"+parent)
	first.recordLogicalAgentAttach("conv")
	if first.readTrackerFor(ctxConv) != first.readTracker {
		t.Fatal("precondition: alone on the connection, the conversation reads on the connection's tracker")
	}
	first.readTrackerFor(ctxConv).Record(notes, readAt, "sha-notes")
	first.recordLogicalAgentAttach("conv/sub") // the subagent joins afterwards
	first.close()

	second := newPersistSession(t, store, ss, "proxy-shared-tracker")
	second.attachWorkspace(context.Background(), "file://"+parent)
	if got := second.readTracker.Mtime(notes); !got.Equal(readAt) {
		t.Fatalf("precondition: the connection's tracker reloaded mtime %v for %s, want the parent's read %v", got, notes, readAt)
	}
	second.recordLogicalAgentCall("conv/sub")

	if second.readTrackerFor(ctxSub) == second.readTracker {
		t.Fatal("the subagent runs on the CONNECTION's read tracker, so the parent's reloaded read record satisfies its strict-mode checks")
	}
	if got := second.readTrackerFor(ctxSub).Mtime(notes); !got.IsZero() {
		t.Errorf("the subagent's own tracker holds mtime %v for %s, a file only its parent read", got, notes)
	}
}

// TestALegacyFollowerRowIsNotRestoredAsAChosenPin: a per-agent row with origin
// roots can only have been written by a release that persisted the connection's
// root for a shard it had merely dragged along (#527). It is not a root the agent
// chose, so the agent must come back following the connection, as it would had
// the row never been written, and a later connection move must take it along.
// Both routes a row is read by are covered: the agent's own, and its
// conversation's, which anchors a subagent.
func TestALegacyFollowerRowIsNotRestoredAsAChosenPin(t *testing.T) {
	cases := []struct {
		name     string
		rowOwner string // whose legacy follower row exists
		caller   string // who calls after the restart
	}{
		{"the agent's own row", "sub", "sub"},
		{"its conversation's row", "conv", "conv/sub"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, ss := newOriginStore(t)
			rootX, rootZ := freshTempDir(t), freshTempDir(t)
			mustGitDir(t, rootX)
			mustGitDir(t, rootZ)
			proxyID := "proxy-legacy-follower-" + string(rune('a'+i))
			ctxCaller := mcp.WithLogicalAgent(context.Background(), c.caller)

			// Before the restart: two agents declared within the window, and the
			// row an older release left for a follower.
			first := newPersistSession(t, store, ss, proxyID)
			first.attachWorkspace(context.Background(), "file://"+rootX)
			first.recordLogicalAgentAttach("coordinator")
			first.recordLogicalAgentAttach(c.caller)
			first.close()
			if err := ss.UpsertPinForAgent(proxyID, c.rowOwner, rootX, "", sessionstate.PinSourceRoots); err != nil {
				t.Fatalf("setup: the legacy row: %v", err)
			}

			calls := 0
			second := newPersistSession(t, store, ss, proxyID)
			second.attachOnInit(context.Background(), rootsReplying(rootX, &calls))
			second.recordLogicalAgentCall(c.caller)
			if got := second.workspaceFor(ctxCaller); got != rootX {
				t.Fatalf("precondition: the agent resolves to %q, want the connection's %q", got, rootX)
			}

			second.onRootsChanged(context.Background(), []string{"file://" + rootZ})
			if got := second.workspace(); got != rootZ {
				t.Fatalf("precondition: the connection moved to %q, not %q", got, rootZ)
			}
			if got := second.workspaceFor(ctxCaller); got != rootZ {
				t.Errorf("the agent resolves to %q after the connection moved to %q: a row it never wrote pinned it in place", got, rootZ)
			}
		})
	}
}
