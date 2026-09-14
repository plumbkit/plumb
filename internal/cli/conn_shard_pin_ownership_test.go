package cli

// conn_shard_pin_ownership_test.go — a shard must be able to tell a workspace
// its agent CHOSE from one it was merely SEEDED with (issue #468).
//
// A shard is seeded from the connection's pin AND from that pin's origin, so a
// connection whose origin some other caller's same-root session_start had
// promoted handed every shard built afterwards a PinSourceSessionStart it never
// asked for. The per-agent sticky guard then refused that agent's FIRST
// explicit pin as a drift away from a workspace it had never held, with
// force: true — which displaces a peer on a pooled connection — as the only way
// through. This is the path the reported session took.
//
// Until the refused pin lands, the agent's workspace-relative calls keep
// resolving inside the seeded root, with nothing in the response saying so. The
// fixture is the reported shape — a git worktree UNDER its parent checkout —
// because that containment is why the drift stayed silent: the same relative
// path exists in both roots, so the wrong root returned a plausible file
// instead of a boundary error.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// worktreeUnderParent builds a parent checkout and a git worktree nested inside
// it, each holding its own copy of the same relative path with distinguishable
// contents.
func worktreeUnderParent(t *testing.T) (parent, worktree string) {
	t.Helper()
	parent = freshTempDir(t)
	mustGitDir(t, parent)
	worktree = filepath.Join(parent, ".claude", "worktrees", "feature")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitDir(t, worktree)
	mustWrite(t, filepath.Join(parent, "notes.md"), "the parent checkout\n")
	mustWrite(t, filepath.Join(worktree, "notes.md"), "the worktree, already edited\n")
	return parent, worktree
}

// TestSeededShardDoesNotInheritAPeersStickiness is the sequence the production
// daemon log records for the reported incident, step for step.
//
// The connection attached to the parent checkout from the client's roots, so
// its pin origin was PinSourceRoots. A peer then named that same root in an
// explicit session_start, which takes attachOrRepinTo's same-root promotion
// branch and upgrades the CONNECTION's pin origin to PinSourceSessionStart —
// silently, since no root moved and the branch logs nothing. The peer's
// declaration turned the connection shared. When the reporting agent's shard
// was built on its next call, shardFor copied that promoted origin onto the
// shard, and the per-agent sticky guard then refused the agent's FIRST EVER
// explicit pin as though it were drifting away from a workspace it had chosen
// — offering force: true, which on a pooled connection displaces a peer.
//
// Until the agent's pin lands, every workspace-relative call resolves inside
// the seeded root instead. With the worktree nested under the parent, that is
// a real file in the wrong repository rather than a boundary error.
func TestSeededShardDoesNotInheritAPeersStickiness(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)

	s := newPersistSession(t, store, ss, "proxy-seeded")
	// The connection attaches to the parent from the client's reported roots.
	s.attachWorkspace(context.Background(), "file://"+parent)

	// A peer names that same root explicitly, promoting the pin origin.
	ctxPeer := mcp.WithLogicalAgent(context.Background(), "peer")
	if _, err := s.repinWorkspace(ctxPeer, parent, "", false); err != nil {
		t.Fatalf("the peer's same-root session_start: %v", err)
	}
	s.recordLogicalAgentAttach("peer")

	// The reporting agent's FIRST session_start, naming its own worktree. It
	// has never pinned anything on this connection.
	ctxAgent := mcp.WithLogicalAgent(context.Background(), "agent-A")
	root, err := s.repinWorkspace(ctxAgent, worktree, "", false)
	if err != nil {
		t.Fatalf("an agent's first explicit pin was refused off a root it never chose: %v", err)
	}
	if root != worktree {
		t.Fatalf("session_start echoed %q, want %q", root, worktree)
	}
	if got := s.workspaceFor(ctxAgent); got != worktree {
		t.Errorf("the agent resolves to %q, want %q — a workspace-relative read would have returned %q",
			got, worktree, filepath.Join(got, "notes.md"))
	}

	// The peer is untouched: per-agent isolation, not a shared pin.
	if got := s.workspaceFor(ctxPeer); got != parent {
		t.Errorf("the peer moved to %q; it must keep %q", got, parent)
	}
	if got := s.workspace(); got != parent {
		t.Errorf("the connection pin moved to %q; it must keep %q", got, parent)
	}
}

// TestChosenShardStaysSticky is the other side of the guard: an agent that
// actually chose a workspace is still refused a non-forced move away from it,
// which is the protection issue #182 asks for.
func TestChosenShardStaysSticky(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)
	other := freshTempDir(t)
	mustGitDir(t, other)

	s := newPersistSession(t, store, ss, "proxy-chosen")
	s.attachWorkspace(context.Background(), "file://"+parent)
	s.recordLogicalAgentAttach("peer")

	ctxAgent := mcp.WithLogicalAgent(context.Background(), "agent-A")
	if _, err := s.repinWorkspace(ctxAgent, worktree, "", false); err != nil {
		t.Fatalf("the agent's own pin: %v", err)
	}

	_, err := s.repinWorkspace(ctxAgent, other, "", false)
	if err == nil {
		t.Fatal("a move away from the workspace this agent chose must be refused without force")
	}
	if !strings.Contains(err.Error(), "sticky") {
		t.Errorf("the refusal lost its diagnosis: %v", err)
	}
	if got := s.workspaceFor(ctxAgent); got != worktree {
		t.Errorf("the refused re-pin moved the shard to %q; it must stay at %q", got, worktree)
	}
}
