package cli

import (
	"sync"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/tools"
)

// newLimiterTestSession builds a minimal connSession wired for rate-limiter
// tests: a store carrying the write limit, a shared clientLimiters map, a fixed
// client identity, the pinned workspace, and a per-session limiter at the same
// limit. It does not register an MCP session (no newConnSession) — only the
// fields bindWriteLimiterParent reads.
func newLimiterTestSession(limiters *sync.Map, client, version, root string, limit int) *connSession {
	base := config.Defaults()
	base.Edits.RateLimitPerMinute = limit
	return &connSession{
		store:          config.NewStore(base),
		clientLimiters: limiters,
		clientName:     client,
		clientVersion:  version,
		acquiredRoot:   root,
		writeLimiter:   tools.NewRateLimiter(limit, time.Minute),
	}
}

// TestBindWriteLimiterParent_DifferentWorkspacesIsolated is the cross-workspace
// isolation contract: two sessions from the SAME client identity but on
// DIFFERENT workspaces must not share a write budget, so a write burst in one
// project never throttles a session in another. This is the behaviour the
// "two different workspaces behave as isolated processes" guarantee rests on.
func TestBindWriteLimiterParent_DifferentWorkspacesIsolated(t *testing.T) {
	var limiters sync.Map
	a := newLimiterTestSession(&limiters, "claude-code", "2.1.0", "/repoA", 2)
	b := newLimiterTestSession(&limiters, "claude-code", "2.1.0", "/repoB", 2)
	a.bindWriteLimiterParent()
	b.bindWriteLimiterParent()

	// Exhaust A's budget (its local window and the shared /repoA budget).
	for i := 0; i < 2; i++ {
		if !a.writeLimiter.Allow() {
			t.Fatalf("write %d on session A should be allowed", i+1)
		}
	}
	if a.writeLimiter.Allow() {
		t.Fatal("third write on session A should be throttled (limit 2)")
	}
	// B is on a different workspace — its shared budget is independent.
	if !b.writeLimiter.Allow() {
		t.Error("a write burst in workspace A must not throttle a session in workspace B")
	}
}

// TestBindWriteLimiterParent_SameWorkspaceShared is the anti-bypass guarantee:
// two connections from the same client identity on the SAME workspace share one
// budget, so a client cannot multiply its write rate by opening several
// connections to one project.
func TestBindWriteLimiterParent_SameWorkspaceShared(t *testing.T) {
	var limiters sync.Map
	a := newLimiterTestSession(&limiters, "claude-code", "2.1.0", "/repoA", 2)
	c := newLimiterTestSession(&limiters, "claude-code", "2.1.0", "/repoA", 2)
	a.bindWriteLimiterParent()
	c.bindWriteLimiterParent()

	// A exhausts the shared (client, workspace) budget.
	for i := 0; i < 2; i++ {
		if !a.writeLimiter.Allow() {
			t.Fatalf("write %d on session A should be allowed", i+1)
		}
	}
	// C shares the same workspace budget, so it is throttled even though its own
	// local window is empty.
	if c.writeLimiter.Allow() {
		t.Error("two connections on the same workspace must share the write budget")
	}
}

// TestBindWriteLimiterParent_NoOpUntilAttached verifies no shared budget is
// created before a workspace is pinned: writes cannot occur pre-attach (the
// boundary guard refuses them), so binding waits until both the client identity
// and the workspace root are known.
func TestBindWriteLimiterParent_NoOpUntilAttached(t *testing.T) {
	var limiters sync.Map
	s := newLimiterTestSession(&limiters, "claude-code", "2.1.0", "", 2)
	s.bindWriteLimiterParent()

	count := 0
	limiters.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Errorf("no shared budget should be created before a workspace is pinned; got %d map entries", count)
	}
}
