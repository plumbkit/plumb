package cli

// conn_first_pin_shard_test.go — the first pin a shared connection ever gets
// (issue #567).
//
// session_start resolves its caller through workspaceFor BEFORE the re-pin, and
// on a shared connection that creates the caller's shard at the connection's
// current root — "" while nothing is pinned. The connection-scoped re-pin then
// moves the connection and drags the shards that follow it, but only the ones
// sitting on the connection's PREVIOUS root, and followConnectionShards returned
// early when there was none. The fresh shard stayed at "" while the report, which
// asks the same followsConnectionLocked predicate, told the caller it worked in
// the new root. The caller's next relative-path call then resolved against
// nothing, contradicting what session_start had just said.

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// TestFirstConnectionPinMovesTheCallersFreshShard: the report and workspaceFor
// agree after a connection's first pin.
func TestFirstConnectionPinMovesTheCallersFreshShard(t *testing.T) {
	store, ss := newOriginStore(t)
	rootX := freshTempDir(t)
	mustGitDir(t, rootX)
	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-A")
	ctxPeer := mcp.WithLogicalAgent(context.Background(), "coordinator")

	s := newPersistSession(t, store, ss, "proxy-first-pin")
	s.recordLogicalAgentAttach("coordinator")
	s.recordLogicalAgentAttach("agent-A")
	// Execute resolves the caller before it re-pins; so does the peer's own call.
	if got := s.workspaceFor(ctxA); got != "" {
		t.Fatalf("precondition: an unpinned connection resolves its caller to %q, want \"\"", got)
	}
	_ = s.workspaceFor(ctxPeer)

	rep, err := s.repinWorkspace(ctxA, rootX, "", false, true)
	if err != nil {
		t.Fatalf("the first connection-scoped session_start: %v", err)
	}

	if rep.Effective != rootX {
		t.Fatalf("session_start reports the caller working in %q, want %q", rep.Effective, rootX)
	}
	if got := s.workspaceFor(ctxA); got != rep.Effective {
		t.Errorf("session_start told the caller it works in %q but its next call resolves to %q: a relative path would resolve against no workspace",
			rep.Effective, got)
	}
	if got := s.workspaceFor(ctxPeer); got != rootX {
		t.Errorf("the peer's shard, which follows the connection, resolves to %q, want %q", got, rootX)
	}
	if rep.Followers != 1 {
		t.Errorf("Followers = %d, want 1 (the peer; the caller is not one of the OTHER agents)", rep.Followers)
	}
}

// TestFirstConnectionPinLeavesAnAgentsOwnPinAlone is the control, in the direction
// a too-greedy fix would break: an agent that chose a root of its own before the
// connection had any is not dragged by the connection's first pin.
func TestFirstConnectionPinLeavesAnAgentsOwnPinAlone(t *testing.T) {
	store, ss := newOriginStore(t)
	rootX, rootW := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootX)
	mustGitDir(t, rootW)
	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-A")
	ctxB := mcp.WithLogicalAgent(context.Background(), "agent-B")

	s := newPersistSession(t, store, ss, "proxy-first-pin-own")
	s.recordLogicalAgentAttach("agent-A")
	s.recordLogicalAgentAttach("agent-B")
	if _, err := s.repinWorkspace(ctxB, rootW, "", false, false); err != nil {
		t.Fatalf("setup: agent-B's own pin on an unpinned connection: %v", err)
	}
	_ = s.workspaceFor(ctxA)

	if _, err := s.repinWorkspace(ctxA, rootX, "", false, true); err != nil {
		t.Fatalf("the first connection-scoped session_start: %v", err)
	}
	if got := s.workspaceFor(ctxB); got != rootW {
		t.Errorf("the connection's first pin dragged an agent that chose its own root to %q, want %q", got, rootW)
	}
	if got := s.workspaceFor(ctxA); got != rootX {
		t.Errorf("the caller resolves to %q, want %q", got, rootX)
	}
}

// TestFirstConnectionPinOnlyMovesShardsSittingAtNoWorkspace pins the precondition
// the fix kept: a connection move drags the shards that sit on the root it LEFT,
// and on a first pin that root is "". A shard that follows the connection but sits
// somewhere else (a seeded one the connection's last move did not reach) is not
// taken to the new root, because nothing says it is meant to be there.
func TestFirstConnectionPinOnlyMovesShardsSittingAtNoWorkspace(t *testing.T) {
	store, ss := newOriginStore(t)
	rootX, rootElsewhere := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootX)
	mustGitDir(t, rootElsewhere)
	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-A")
	ctxB := mcp.WithLogicalAgent(context.Background(), "agent-B")

	s := newPersistSession(t, store, ss, "proxy-first-pin-only")
	s.recordLogicalAgentAttach("agent-A")
	s.recordLogicalAgentAttach("agent-B")
	_ = s.workspaceFor(ctxA)
	_ = s.workspaceFor(ctxB)
	// agent-B follows the connection (it chose nothing) yet sits elsewhere.
	shB := s.shardFor(ctxB)
	shB.mu.Lock()
	shB.root = rootElsewhere
	shB.mu.Unlock()

	if _, err := s.repinWorkspace(ctxA, rootX, "", false, true); err != nil {
		t.Fatalf("the first connection-scoped session_start: %v", err)
	}
	if got := s.workspaceFor(ctxA); got != rootX {
		t.Errorf("the shard at no workspace resolves to %q, want %q", got, rootX)
	}
	if got := s.workspaceFor(ctxB); got != rootElsewhere {
		t.Errorf("a following shard that sat elsewhere was taken to %q; only shards on the root the move left are dragged", got)
	}
}
