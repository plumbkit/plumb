package cli

// conn_shard_follow_test.go — PLAN-398: a shard seeded from the connection pin
// belongs to the connection until its agent deliberately re-pins it. shardFor
// caches the shard BEFORE repinAgent can refuse, so one refused ask left the
// agent cached at a stale root whose sticky seed then refused the agent's next,
// entirely legitimate call — while a fresh agent asking the identical thing
// succeeded. The fix: when the connection itself moves, every shard still
// living where the connection seeded it follows; a shard whose agent chose its
// own root does not.

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
)

// TestShardSeededBeforeRefusalFollowsTheConnection is the card's five-step
// reproduction, verbatim.
func TestShardSeededBeforeRefusalFollowsTheConnection(t *testing.T) {
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

	// 2. sub asks for Y and is refused — but its shard is now cached at X.
	if _, err := s.repinWorkspace(ctxSub, rootY, "", false); err == nil {
		t.Fatal("precondition: sub's cross-workspace re-pin should have been refused")
	}
	if got := s.workspaceFor(ctxSub); got != rootX {
		t.Fatalf("precondition: after the refusal sub resolves to %q, want the seeded %q", got, rootX)
	}

	// 3. The connection legitimately moves to Z (force: the pin is sticky).
	if _, err := s.repinWorkspace(context.Background(), rootZ, "", true); err != nil {
		t.Fatalf("connection move to Z: %v", err)
	}

	// 4. THE CARD: sub now asks for Z — where the connection actually is — and
	// must be ACCEPTED. Before the fix the stale cached shard's sticky seed at
	// X refused the identical request its peer's fresh shard admitted.
	if _, err := s.repinWorkspace(ctxSub, rootZ, "", false); err != nil {
		t.Fatalf("a legitimate call for the root the connection moved to was refused off a stale seeded shard (PLAN-398): %v", err)
	}
	if got := s.workspaceFor(ctxSub); got != rootZ {
		t.Fatalf("sub resolves to %q, want %q", got, rootZ)
	}

	// 5. Control: a fresh agent asking the identical thing is accepted too.
	if _, err := s.repinWorkspace(ctxFresh, rootZ, "", false); err != nil {
		t.Fatalf("control: a fresh agent's identical call must be accepted: %v", err)
	}

	// Fail-closed survives the follow: sub's shard now lives at Z, so a genuine
	// cross-workspace ask is still refused, with the remedy.
	_, driftErr := s.repinWorkspace(ctxSub, rootY, "", false)
	if driftErr == nil {
		t.Fatal("after following the connection, a genuine cross-workspace drift must still be refused")
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

	// sub's first ask is refused (shard seeded at X), then it CHOOSES W with force.
	if _, err := s.repinWorkspace(ctxSub, rootW, "", false); err == nil {
		t.Fatal("precondition: the cross-workspace ask should have been refused")
	}
	if _, err := s.repinWorkspace(ctxSub, rootW, "", true); err != nil {
		t.Fatalf("sub's forced pin to W: %v", err)
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
