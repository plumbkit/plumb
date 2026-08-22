package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
)

// TestAgentRepinIsolation is the headline acceptance for PLAN-286: two logical
// agents multiplexed over one connection re-pin independently, so B's
// session_start does not reset A's pin, and the per-agent sticky guard refuses a
// SAME-agent non-forced re-pin while a different agent's re-pin lands on its own
// shard (the actual issue #182 fix).
func TestAgentRepinIsolation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	// Two distinct logical agents share one connection.
	s.recordLogicalAgentAttach("agent-a")
	s.recordLogicalAgentCall("agent-b")

	rootA := freshTempDir(t)
	mustGitDir(t, rootA)
	rootB := freshTempDir(t)
	mustGitDir(t, rootB)
	rootC := freshTempDir(t)
	mustGitDir(t, rootC)

	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")
	ctxB := mcp.WithLogicalAgent(context.Background(), "agent-b")

	if _, err := s.repinWorkspace(ctxA, "file://"+rootA, "", false); err != nil {
		t.Fatalf("agent A re-pin: %v", err)
	}
	if _, err := s.repinWorkspace(ctxB, "file://"+rootB, "", false); err != nil {
		t.Fatalf("agent B re-pin: %v", err)
	}
	if got := s.workspaceFor(ctxA); got != rootA {
		t.Fatalf("agent A workspace = %q, want %q — B clobbered A's pin", got, rootA)
	}
	if got := s.workspaceFor(ctxB); got != rootB {
		t.Fatalf("agent B workspace = %q, want %q", got, rootB)
	}

	// A same-agent non-forced re-pin is refused (sticky, inverted guard).
	if _, err := s.repinWorkspace(ctxA, "file://"+rootC, "", false); err == nil {
		t.Fatal("same-agent non-forced re-pin was accepted; the per-agent sticky guard must refuse it")
	}
	if got := s.workspaceFor(ctxA); got != rootA {
		t.Fatalf("refused re-pin moved agent A's pin to %q, want %q", got, rootA)
	}
	// A forced same-agent re-pin lands.
	if _, err := s.repinWorkspace(ctxA, "file://"+rootC, "", true); err != nil {
		t.Fatalf("forced same-agent re-pin: %v", err)
	}
	if got := s.workspaceFor(ctxA); got != rootC {
		t.Fatalf("forced re-pin left agent A at %q, want %q", got, rootC)
	}
	// Agent B is untouched by A's forced move.
	if got := s.workspaceFor(ctxB); got != rootB {
		t.Fatalf("agent B workspace = %q after A moved, want %q", got, rootB)
	}
}

// TestAgentTrackerIsolation pins that the per-agent read trackers are separate:
// one agent's re-pin resets its own trackers, never a peer's.
func TestAgentTrackerIsolation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	s.recordLogicalAgentAttach("agent-a")
	s.recordLogicalAgentCall("agent-b")

	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")
	ctxB := mcp.WithLogicalAgent(context.Background(), "agent-b")

	rootA := freshTempDir(t)
	mustGitDir(t, rootA)
	rootB := freshTempDir(t)
	mustGitDir(t, rootB)
	if _, err := s.repinWorkspace(ctxA, "file://"+rootA, "", false); err != nil {
		t.Fatalf("agent A re-pin: %v", err)
	}
	if _, err := s.repinWorkspace(ctxB, "file://"+rootB, "", false); err != nil {
		t.Fatalf("agent B re-pin: %v", err)
	}

	// Agent A records a read.
	path := rootA + "/a.go"
	mtime := time.Unix(1_700_000_000, 0)
	s.readTrackerFor(ctxA).Record(path, mtime, "sha-a")

	// Force B to a new root to exercise B's own reset path.
	rootB2 := freshTempDir(t)
	mustGitDir(t, rootB2)
	if _, err := s.repinWorkspace(ctxB, "file://"+rootB2, "", true); err != nil {
		t.Fatalf("agent B forced re-pin: %v", err)
	}
	if got := s.readTrackerFor(ctxA).Mtime(path); !got.Equal(mtime) {
		t.Fatalf("agent B's re-pin reset agent A's read tracker (got %v)", got)
	}
	if got := s.workspaceFor(ctxA); got != rootA {
		t.Fatalf("agent A workspace = %q after B moved, want %q", got, rootA)
	}
	if got := s.workspaceFor(ctxB); got != rootB2 {
		t.Fatalf("agent B workspace = %q, want %q", got, rootB2)
	}
}

// TestWriteDepsRoutePerAgent proves the live tool path resolves per-agent state
// (PLAN-286): buildWriteDeps wires the ctx-aware resolvers, so a read recorded
// through the resolver the tools use lands in that agent's shard tracker, not a
// peer's, and distinct agents get distinct trackers and rate limiters.
func TestWriteDepsRoutePerAgent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	s.recordLogicalAgentAttach("agent-a")
	s.recordLogicalAgentCall("agent-b")

	wd := s.buildWriteDeps()
	if wd.ReadsFor == nil || wd.WritesFor == nil || wd.UndoFor == nil || wd.LimiterFor == nil {
		t.Fatal("buildWriteDeps must wire the per-agent resolvers")
	}

	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")
	ctxB := mcp.WithLogicalAgent(context.Background(), "agent-b")

	ra, rb := wd.ReadsFor(ctxA), wd.ReadsFor(ctxB)
	if ra == rb {
		t.Fatal("agents must have separate read trackers")
	}
	if wd.LimiterFor(ctxA) == wd.LimiterFor(ctxB) {
		t.Fatal("agents must have separate rate limiters")
	}

	// A read recorded through the resolver the tools use lands only in that
	// agent's tracker, never a peer's.
	ra.Record("/ws/a.go", time.Unix(1_700_000_000, 0), "sha-a")
	if s.readTrackerFor(ctxA).Mtime("/ws/a.go").IsZero() {
		t.Fatal("read recorded via ReadsFor must land in agent A's tracker")
	}
	if !s.readTrackerFor(ctxB).Mtime("/ws/a.go").IsZero() {
		t.Fatal("agent A's read leaked into agent B's tracker")
	}
}

// TestAgentRepinRefusalLeavesATrace pins the lifecycle of the per-agent
// refusal's operator-visible mark. The connection-level guard has always
// recorded a refused steal (markBoundaryViolation); when the refusal moved
// per-agent it must not go silent, because a refused cross-workspace drift on a
// shared connection is exactly what an operator needs to see on this
// past-vulnerability surface. It must also not outlive the condition.
//
// This is a unit test rather than an assertion inside the integration harness
// because markSharedConnectionDetected re-fires — and overwrites Health — on
// every subsequent identity declaration, which masks the heal end-to-end.
func TestAgentRepinRefusalLeavesATrace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	s.recordLogicalAgentCall("agent-a")
	s.recordLogicalAgentCall("agent-b")
	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")

	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	if _, err := s.repinWorkspace(ctxA, "file://"+rootA, "", false); err != nil {
		t.Fatalf("agent A first pin: %v", err)
	}

	if _, err := s.repinWorkspace(ctxA, "file://"+rootB, "", false); err == nil {
		t.Fatal("precondition: the same-agent non-forced re-pin should have been refused")
	}
	health, msg := sessionHealth(t, s.sessID)
	if health != agentRepinRefusedHealth {
		t.Fatalf("health = %q after a refused per-agent re-pin, want %q — the refusal left no trace",
			health, agentRepinRefusedHealth)
	}
	if health == "blocked" {
		t.Error(`health = "blocked": one agent's scoping question must not flag the whole connection`)
	}
	for _, want := range []string{"agent-a", rootA, rootB, "force: true"} {
		if !strings.Contains(msg, want) {
			t.Errorf("health message does not name %q, so an operator cannot act on it: %s", want, msg)
		}
	}

	// The agent's own successful re-pin heals it.
	if _, err := s.repinWorkspace(ctxA, "file://"+rootB, "", true); err != nil {
		t.Fatalf("forced re-pin: %v", err)
	}
	if health, msg := sessionHealth(t, s.sessID); health == agentRepinRefusedHealth {
		t.Errorf("the refusal mark survived the agent's own successful re-pin: %s", msg)
	}
}
