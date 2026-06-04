package cli

import (
	"sync"
	"time"

	"github.com/golimpio/plumb/internal/tools"
)

// sharedBudgets is the registry of write-rate budgets shared across connections
// from the same (MCP client identity, workspace) pair. Connections that share a
// key share one RateLimiter, so a client cannot multiply its allowed write rate
// by opening several connections to one workspace; connections on different
// workspaces never share, preserving cross-workspace isolation.
//
// Entries are reference-counted by the sessions that bind them (mirroring
// poolEntry.refs): the entry is removed when its last session releases it, so a
// long-running daemon that touches many workspaces does not accumulate budgets
// forever.
//
// Concurrency: all methods are safe for concurrent use; mu guards the map and
// every entry's refs. The limiter inside an entry is itself internally locked.
type sharedBudgets struct {
	mu sync.Mutex
	m  map[string]*budgetEntry
}

type budgetEntry struct {
	limiter *tools.RateLimiter
	refs    int
}

func newSharedBudgets() *sharedBudgets {
	return &sharedBudgets{m: make(map[string]*budgetEntry)}
}

// acquire returns the shared limiter for key, creating it on first use and
// incrementing its reference count. The limiter's cap is (re)set to limit on
// every acquire, so a config reload that changes the rate propagates to the
// shared budget rather than keeping the value it was first created with.
func (b *sharedBudgets) acquire(key string, limit int) *tools.RateLimiter {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.m[key]
	if !ok {
		e = &budgetEntry{limiter: tools.NewRateLimiter(limit, time.Minute)}
		b.m[key] = e
	}
	e.refs++
	e.limiter.SetLimit(limit)
	return e.limiter
}

// setLimit refreshes the cap of an already-acquired budget without changing its
// refcount. Used on a config reload by a session still bound to the same key.
// No-op when the key is gone.
func (b *sharedBudgets) setLimit(key string, limit int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.m[key]; ok {
		e.limiter.SetLimit(limit)
	}
}

// release drops one reference on key, removing the entry when its last session
// leaves. A no-op for an unknown key or one already at zero (defensive against a
// double release).
func (b *sharedBudgets) release(key string) {
	if b == nil || key == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.m[key]
	if !ok || e.refs <= 0 {
		return
	}
	e.refs--
	if e.refs == 0 {
		delete(b.m, key)
	}
}

// len reports the number of live budget entries. Test-only helper.
func (b *sharedBudgets) len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.m)
}
