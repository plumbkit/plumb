package cli

// conn_shard_follow_test.go — PLAN-398: a shard seeded from the connection pin
// belongs to the connection until its agent deliberately re-pins it. shardFor
// caches the shard from the connection's CURRENT pin and never revisits it, so
// an agent seeded before the connection moved was left resolving against a root
// the connection had since left, while a fresh agent asking the identical thing
// resolved correctly. The fix: when the connection itself moves, every shard
// still living where the connection seeded it follows; a shard whose agent
// chose its own root does not.
//
// The card's original reproduction seeded the shard by making an ask that the
// per-agent sticky guard REFUSED, because the guard used to fire on a seeded
// shard's inherited pin origin. It no longer does (issue #468: a shard that
// chose nothing is not a deliberate pin, and offering force: true as the way
// out of one displaces a peer). The seeding below is therefore an ordinary
// workspace resolution, which is what creates the shard in production too, and
// the fail-closed tail now runs against a shard whose agent really did choose.

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
)

// TestShardSeededFromTheConnectionFollowsIt is the card's reproduction: a shard
// seeded where the connection was must not be left behind when it moves.
func TestShardSeededFromTheConnectionFollowsIt(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	rootX := freshTempDir(t)
	mustGitDir(t, rootX)
	rootY := freshTempDir(t)
	mustGitDir(t, rootY)
	rootZ := freshTempDir(t)
	mustGitDir(t, rootZ)

	ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")
	ctxFresh := mcp.WithLogicalAgent(context.Background(), "fresh")

	// 1. The connection pins to X (unattributed call — the connection-level path).
	if _, err := s.repinWorkspace(context.Background(), rootX, "", false); err != nil {
		t.Fatalf("connection pin to X: %v", err)
	}
	s.recordLogicalAgentAttach("coordinator")
	s.recordLogicalAgentAttach("sub")

	// 2. sub resolves a workspace, which caches its shard at X.
	if got := s.workspaceFor(ctxSub); got != rootX {
		t.Fatalf("precondition: sub's shard seeded at %q, want the connection's %q", got, rootX)
	}

	// 3. The connection legitimately moves to Z (force: the pin is sticky).
	if _, err := s.repinWorkspace(context.Background(), rootZ, "", true); err != nil {
		t.Fatalf("connection move to Z: %v", err)
	}

	// 4. THE CARD: sub's seeded shard follows the connection rather than being
	// stranded at X, so its next call resolves where the connection actually is.
	if got := s.workspaceFor(ctxSub); got != rootZ {
		t.Fatalf("the seeded shard was stranded at %q after the connection moved to %q (PLAN-398)", got, rootZ)
	}
	if _, err := s.repinWorkspace(ctxSub, rootZ, "", false); err != nil {
		t.Fatalf("a legitimate call for the root the connection moved to was refused: %v", err)
	}

	// 5. Control: a fresh agent asking the identical thing is accepted too.
	if _, err := s.repinWorkspace(ctxFresh, rootZ, "", false); err != nil {
		t.Fatalf("control: a fresh agent's identical call must be accepted: %v", err)
	}

	// Fail-closed survives the follow: sub's call above NAMED Z explicitly, and
	// naming the root a shard already holds is a choice like any other, so a
	// genuine cross-workspace drift is now refused with the diagnosis and remedy.
	_, driftErr := s.repinWorkspace(ctxSub, rootY, "", false)
	if driftErr == nil {
		t.Fatal("after naming a root, a genuine cross-workspace drift must be refused")
	}
	if !strings.Contains(driftErr.Error(), "force") && !strings.Contains(driftErr.Error(), "sticky") {
		t.Errorf("the drift refusal lost its diagnosis and remedy: %v", driftErr)
	}
}

// TestSelfPinnedShardDoesNotFollowTheConnection: an agent that CHOSE its own
// workspace keeps it when the connection moves — per-agent isolation is the
// point of the shard machinery, so the follow must reach only shards still
// living where the connection seeded them.
func TestSelfPinnedShardDoesNotFollowTheConnection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	rootX := freshTempDir(t)
	mustGitDir(t, rootX)
	rootW := freshTempDir(t)
	mustGitDir(t, rootW)
	rootZ := freshTempDir(t)
	mustGitDir(t, rootZ)

	ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")

	if _, err := s.repinWorkspace(context.Background(), rootX, "", false); err != nil {
		t.Fatalf("connection pin to X: %v", err)
	}
	s.recordLogicalAgentAttach("coordinator")
	s.recordLogicalAgentAttach("sub")

	// sub's shard is seeded at X, then sub CHOOSES W.
	if got := s.workspaceFor(ctxSub); got != rootX {
		t.Fatalf("precondition: sub's shard seeded at %q, want %q", got, rootX)
	}
	if _, err := s.repinWorkspace(ctxSub, rootW, "", false); err != nil {
		t.Fatalf("sub's pin to W: %v", err)
	}

	// The connection moves to Z. sub's own choice must survive it.
	if _, err := s.repinWorkspace(context.Background(), rootZ, "", true); err != nil {
		t.Fatalf("connection move to Z: %v", err)
	}
	if got := s.workspaceFor(ctxSub); got != rootW {
		t.Fatalf("a self-pinned shard was dragged to %q by the connection's move — per-agent isolation must keep it at %q", got, rootW)
	}
	if got := s.workspace(); got != rootZ {
		t.Fatalf("connection pin = %q, want %q", got, rootZ)
	}

	// The load-bearing sequence for the selfPinned guard: the connection moves
	// THROUGH the agent's chosen root and away again. Each hop must leave the
	// shard at W — without the guard, the move away from W re-seeds the shard
	// (its root matches the connection's previous root), dragging the agent's
	// own choice along with the connection.
	if _, err := s.repinWorkspace(context.Background(), rootW, "", true); err != nil {
		t.Fatalf("connection move to W: %v", err)
	}
	if got := s.workspaceFor(ctxSub); got != rootW {
		t.Fatalf("connection settling on the agent's own root moved the shard to %q, want %q", got, rootW)
	}
	if _, err := s.repinWorkspace(context.Background(), rootZ, "", true); err != nil {
		t.Fatalf("connection move away from W: %v", err)
	}
	if got := s.workspaceFor(ctxSub); got != rootW {
		t.Fatalf("the move away from W dragged the self-pinned shard to %q — an agent's own choice survives the connection passing through it (PLAN-398)", got)
	}
}
