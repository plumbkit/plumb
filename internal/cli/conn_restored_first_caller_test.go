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
