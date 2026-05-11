package tools

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// RateLimiter is a sliding-window rate limiter used to cap how many write
// operations a single MCP session can issue per minute. The default is
// permissive (120/min) — it exists to protect against a runaway loop in
// the agent, not to throttle normal use.
//
// Concurrency: Allow is safe for concurrent use.
type RateLimiter struct {
	mu     sync.Mutex
	stamps []time.Time
	limit  int
	window time.Duration
}

// NewRateLimiter creates a limiter that allows up to limit operations per
// window. limit <= 0 disables limiting (Allow always returns true).
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{limit: limit, window: window}
}

// Allow reports whether one operation is permitted right now. If true, the
// operation is recorded against the window. If false, the caller should
// refuse the operation; nothing is recorded.
func (r *RateLimiter) Allow() bool {
	if r == nil || r.limit <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-r.window)
	// Evict timestamps older than the window.
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
	return true
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
