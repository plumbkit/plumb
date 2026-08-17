package cli

import (
	"context"
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
