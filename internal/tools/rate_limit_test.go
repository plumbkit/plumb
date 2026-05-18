package tools

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsUpToLimit(t *testing.T) {
	r := NewRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !r.Allow() {
			t.Fatalf("Allow #%d = false, want true", i+1)
		}
	}
	if r.Allow() {
		t.Fatal("Allow #4 should have been throttled")
	}
}

func TestRateLimiter_RecoversAfterWindow(t *testing.T) {
	r := NewRateLimiter(2, 50*time.Millisecond)
	if !r.Allow() {
		t.Fatal("Allow #1 = false")
	}
	if !r.Allow() {
		t.Fatal("Allow #2 = false")
	}
	if r.Allow() {
		t.Fatal("Allow #3 should have been throttled")
	}
	time.Sleep(60 * time.Millisecond)
	if !r.Allow() {
		t.Fatal("Allow after window expiry should succeed")
	}
}

func TestRateLimiter_ZeroLimit_Unlimited(t *testing.T) {
	r := NewRateLimiter(0, time.Minute)
	for i := 0; i < 100; i++ {
		if !r.Allow() {
			t.Fatalf("Allow #%d on zero-limit limiter = false", i+1)
		}
	}
}

func TestRateLimiter_NilLimiter_Unlimited(t *testing.T) {
	var r *RateLimiter
	if !r.Allow() {
		t.Fatal("nil limiter should allow")
	}
}

// TestRateLimiter_SharedParent_ExhaustsAcrossConnections is the primary
// correctness test for the per-client-identity budget: two connections share
// one parent limiter with budget 3. The total write count across both
// connections must not exceed 3, even though each per-connection limiter
// would allow 10 on its own.
func TestRateLimiter_SharedParent_ExhaustsAcrossConnections(t *testing.T) {
	parent := NewRateLimiter(3, time.Minute) // shared client budget

	connA := NewRateLimiter(10, time.Minute) // connection A — high local limit
	connA.SetParent(parent)
	connB := NewRateLimiter(10, time.Minute) // connection B — same client
	connB.SetParent(parent)

	allowed := 0
	for _, r := range []*RateLimiter{connA, connA, connB, connB, connA} {
		if r.Allow() {
			allowed++
		}
	}

	if allowed != 3 {
		t.Fatalf("allowed = %d, want 3 (shared parent budget exhausted after 3)", allowed)
	}
}

// TestRateLimiter_PerConnectionLimitStillApplies ensures the per-connection
// window still triggers independently of the parent — the smaller constraint wins.
func TestRateLimiter_PerConnectionLimitStillApplies(t *testing.T) {
	parent := NewRateLimiter(10, time.Minute) // generous parent
	conn := NewRateLimiter(2, time.Minute)    // tight per-connection limit
	conn.SetParent(parent)

	if !conn.Allow() {
		t.Fatal("first call should be allowed")
	}
	if !conn.Allow() {
		t.Fatal("second call should be allowed")
	}
	if conn.Allow() {
		t.Fatal("third call should be blocked by per-connection limit")
	}
	// Parent should only have 2 consumed, not 3.
	count, _, _ := parent.Snapshot()
	if count != 2 {
		t.Fatalf("parent consumed %d slots, want 2", count)
	}
}

// TestRateLimiter_SetParentNil detaches the parent and makes the limiter
// standalone; subsequent calls do not consult the old parent.
func TestRateLimiter_SetParentNil(t *testing.T) {
	parent := NewRateLimiter(1, time.Minute) // exhausted after first call
	conn := NewRateLimiter(10, time.Minute)
	conn.SetParent(parent)

	conn.Allow() // exhausts parent

	// With parent still attached the next call must be blocked.
	if conn.Allow() {
		t.Fatal("expected block from exhausted parent")
	}

	// Detach parent — now standalone.
	conn.SetParent(nil)
	if !conn.Allow() {
		t.Fatal("expected allow after parent detached")
	}
}

func TestRateLimiter_ZeroLocalLimitBypassesParent(t *testing.T) {
	parent := NewRateLimiter(1, time.Minute)
	conn := NewRateLimiter(0, time.Minute)
	conn.SetParent(parent)

	if !conn.Allow() {
		t.Fatal("zero local limit should allow first call")
	}
	if !conn.Allow() {
		t.Fatal("zero local limit should disable rate limiting for this connection, including the shared parent")
	}
}
