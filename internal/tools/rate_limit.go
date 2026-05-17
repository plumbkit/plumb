package tools

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// RateLimiter is a sliding-window rate limiter used to cap how many write
// operations a single MCP session can issue per minute. The default is
// permissive (120/min) — it exists to protect against a runaway loop in
// the agent, not to throttle normal use.
//
// A limiter may have an optional parent (set via SetParent). If a parent is
// set, Allow checks the parent's shared budget BEFORE recording a local slot.
// This enables daemon-scoped, per-client-identity budgets: each connection
// keeps its own per-connection limiter (so per-project config changes remain
// isolated), but shares a parent budget with all other connections from the
// same MCP client, preventing limit bypass by opening multiple connections.
//
// Concurrency: all methods are safe for concurrent use.
type RateLimiter struct {
	mu     sync.Mutex
	stamps []time.Time
	limit  int
	window time.Duration
	parent atomic.Pointer[RateLimiter] // optional shared budget; nil means standalone
}

// NewRateLimiter creates a limiter that allows up to limit operations per
// window. limit <= 0 disables limiting (Allow always returns true).
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{limit: limit, window: window}
}

// SetParent attaches a shared parent budget to this limiter. Subsequent Allow
// calls will check the parent before recording a local slot. Pass nil to
// detach the parent and make this limiter standalone again.
func (r *RateLimiter) SetParent(parent *RateLimiter) {
	if r == nil {
		return
	}
	if parent == nil {
		r.parent.Store(nil)
	} else {
		r.parent.Store(parent)
	}
}

// Allow reports whether one operation is permitted right now. If true, the
// operation is recorded against both the local window and the parent budget
// (when one is set). If false, the caller should refuse the operation;
// nothing is recorded.
//
// Checking order: the local window is evaluated first (no side-effects until
// both checks pass). The parent is then checked without holding the local
// lock (to avoid a lock-chain between sibling limiters). The local lock is
// re-acquired and the window is re-verified before recording the stamp.
func (r *RateLimiter) Allow() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()

	// Evaluate the local window.
	localOK := true
	if r.limit > 0 {
		now := time.Now()
		cutoff := now.Add(-r.window)
		i := 0
		for i < len(r.stamps) && r.stamps[i].Before(cutoff) {
			i++
		}
		if i > 0 {
			r.stamps = r.stamps[i:]
		}
		if len(r.stamps) >= r.limit {
			localOK = false
		}
	}
	if !localOK {
		r.mu.Unlock()
		return false
	}

	// Check parent without holding our lock (prevents lock ordering issues).
	p := r.parent.Load()
	r.mu.Unlock()
	if p != nil && !p.Allow() {
		return false
	}

	// Re-acquire and re-verify the local window, then record the slot.
	// The window may have advanced while we were checking the parent.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.limit > 0 {
		now := time.Now()
		cutoff := now.Add(-r.window)
		i := 0
		for i < len(r.stamps) && r.stamps[i].Before(cutoff) {
			i++
		}
		if i > 0 {
			r.stamps = r.stamps[i:]
		}
		if len(r.stamps) >= r.limit {
			return false
		}
		r.stamps = append(r.stamps, now)
	}
	return true
}

// SetLimit updates the limit (operations per current window). Setting limit
// to 0 disables limiting. Called by the daemon when a project-local config
// resolves and the rate limit changes from the global default.
func (r *RateLimiter) SetLimit(limit int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.limit = limit
	r.mu.Unlock()
}

// Snapshot returns the current count and the window duration. Used by tests
// and by error messages to explain what was exceeded.
func (r *RateLimiter) Snapshot() (count int, limit int, window time.Duration) {
	if r == nil {
		return 0, 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stamps), r.limit, r.window
}

// defaultWriteRateLimit returns the rate-limit configuration from environment:
//
//	PLUMB_WRITE_RATE_LIMIT — integer, ops per minute. 0 disables. Default 120.
func defaultWriteRateLimit() (int, time.Duration) {
	const defaultLimit = 120
	const defaultWindow = time.Minute
	v := os.Getenv("PLUMB_WRITE_RATE_LIMIT")
	if v == "" {
		return defaultLimit, defaultWindow
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultLimit, defaultWindow
	}
	return n, defaultWindow
}

// NewDefaultRateLimiter constructs the limiter the daemon installs on each
// session. Reads PLUMB_WRITE_RATE_LIMIT from env.
func NewDefaultRateLimiter() *RateLimiter {
	limit, window := defaultWriteRateLimit()
	return NewRateLimiter(limit, window)
}

// rateLimitError wraps editLogicErr semantically (don't retry) but with a
// user-friendly message about throttling.
func rateLimitError(tool string, lim *RateLimiter) error {
	count, limit, window := lim.Snapshot()
	return &editLogicErr{fmt.Errorf(
		"%s: rate limit exceeded — %d writes in the last %s (limit %d). "+
			"Slow down or set PLUMB_WRITE_RATE_LIMIT=0 to disable",
		tool, count, window, limit)}
}
