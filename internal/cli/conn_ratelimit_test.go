package cli

import (
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/tools"
)

// newLimiterTestSession builds a minimal connSession wired for rate-limiter
// tests: a store carrying the write limit, the shared budget registry, a fixed
// client identity, the pinned workspace, and a per-session limiter at the same
// limit. It does not register an MCP session (no newConnSession) — only the
// fields bindWriteLimiterParent reads. bindWriteLimiterParent sources the shared
// cap from the per-session limiter's snapshot, so writeLimiter must carry the
// intended limit.
func newLimiterTestSession(budgets *sharedBudgets, client, version, root string, limit int) *connSession {
	base := config.Defaults()
	base.Edits.RateLimitPerMinute = limit
	s := &connSession{
		store:        config.NewStore(base),
		budgets:      budgets,
		writeLimiter: tools.NewRateLimiter(limit, time.Minute),
	}
	s.mutate(func(v *sessionView) {
		v.clientName = client
		v.clientVersion = version
		v.acquiredRoot = root
	})
	return s
}

// TestBindWriteLimiterParent_DifferentWorkspacesIsolated is the cross-workspace
// isolation contract: two sessions from the SAME client identity but on
// DIFFERENT workspaces must not share a write budget, so a write burst in one
// project never throttles a session in another. This is the behaviour the
// "two different workspaces behave as isolated processes" guarantee rests on.
func TestBindWriteLimiterParent_DifferentWorkspacesIsolated(t *testing.T) {
	budgets := newSharedBudgets()
	a := newLimiterTestSession(budgets, "claude-code", "2.1.0", "/repoA", 2)
	b := newLimiterTestSession(budgets, "claude-code", "2.1.0", "/repoB", 2)
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
	budgets := newSharedBudgets()
	a := newLimiterTestSession(budgets, "claude-code", "2.1.0", "/repoA", 2)
	c := newLimiterTestSession(budgets, "claude-code", "2.1.0", "/repoA", 2)
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
	budgets := newSharedBudgets()
	s := newLimiterTestSession(budgets, "claude-code", "2.1.0", "", 2)
	s.bindWriteLimiterParent()

	if n := budgets.len(); n != 0 {
		t.Errorf("no shared budget should be created before a workspace is pinned; got %d entries", n)
	}
}

// TestBindWriteLimiterParent_RepinReleasesOldBudget verifies that re-pinning a
// session to a new workspace releases its reference on the old budget, so the
// old entry is reclaimed rather than leaking — and that a repeat bind on the
// same key never inflates the count.
func TestBindWriteLimiterParent_RepinReleasesOldBudget(t *testing.T) {
	budgets := newSharedBudgets()
	s := newLimiterTestSession(budgets, "claude-code", "2.1.0", "/repoA", 2)
	s.bindWriteLimiterParent()
	if n := budgets.len(); n != 1 {
		t.Fatalf("want 1 budget after first bind; got %d", n)
	}
	// A repeat bind on the same key is a no-op for the count.
	s.bindWriteLimiterParent()
	if n := budgets.len(); n != 1 {
		t.Fatalf("a repeat bind on the same key must not create a budget; got %d", n)
	}
	// Re-pin to a different workspace: the old reference is released.
	s.mutate(func(v *sessionView) { v.acquiredRoot = "/repoB" })
	s.bindWriteLimiterParent()
	if n := budgets.len(); n != 1 {
		t.Fatalf("re-pin must release the old budget; want 1 entry, got %d", n)
	}
}

// TestSharedBudgets_AcquireReleaseRefcount covers the refcounted lifecycle: a
// shared key returns one limiter across acquires and is reclaimed only when its
// last reference is released. Guards against the unbounded-growth leak.
func TestSharedBudgets_AcquireReleaseRefcount(t *testing.T) {
	b := newSharedBudgets()
	l1 := b.acquire("k", 3)
	l2 := b.acquire("k", 3)
	if l1 != l2 {
		t.Fatal("the same key must return the same shared limiter")
	}
	if n := b.len(); n != 1 {
		t.Fatalf("want 1 entry for one key; got %d", n)
	}
	b.release("k")
	if n := b.len(); n != 1 {
		t.Fatalf("entry must survive while a reference remains; got %d", n)
	}
	b.release("k")
	if n := b.len(); n != 0 {
		t.Fatalf("entry must be reclaimed when the last reference leaves; got %d", n)
	}
	b.release("k") // double release is a defensive no-op
}

// TestSharedBudgets_AcquireUpdatesLimit is the config-reload contract: acquiring
// or refreshing a budget tracks the latest cap, so a rate-limit change reaches
// the shared parent instead of being stuck at the value it was first created
// with.
func TestSharedBudgets_AcquireUpdatesLimit(t *testing.T) {
	b := newSharedBudgets()
	l := b.acquire("k", 2)
	if _, lim, _ := l.Snapshot(); lim != 2 {
		t.Fatalf("initial cap should be 2; got %d", lim)
	}
	b.acquire("k", 5) // a second binder with a higher resolved limit
	if _, lim, _ := l.Snapshot(); lim != 5 {
		t.Fatalf("acquire must raise the shared cap to 5; got %d", lim)
	}
	b.setLimit("k", 1) // a same-key reload lowering the limit
	if _, lim, _ := l.Snapshot(); lim != 1 {
		t.Fatalf("setLimit must lower the shared cap to 1; got %d", lim)
	}
}
