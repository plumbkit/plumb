package tools

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimiter_AllowsUpToLimit(t *testing.T) {
	r := NewRateLimiter(3, time.Minute)
	for i := range 3 {
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
	for i := range 100 {
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

// TestRateLimiter_release checks the compensating release: it drops one slot
// and is a safe no-op on an empty or nil limiter.
func TestRateLimiter_release(t *testing.T) {
	r := NewRateLimiter(5, time.Minute)
	r.Allow()
	r.Allow()
	if count, _, _ := r.Snapshot(); count != 2 {
		t.Fatalf("after two Allow calls count = %d, want 2", count)
	}

	r.release()
	if count, _, _ := r.Snapshot(); count != 1 {
		t.Fatalf("after release count = %d, want 1", count)
	}

	// Over-releasing past empty must not underflow.
	r.release()
	r.release()
	if count, _, _ := r.Snapshot(); count != 0 {
		t.Fatalf("after draining count = %d, want 0", count)
	}

	var nilLimiter *RateLimiter
	nilLimiter.release() // must not panic
}

// TestRateLimiter_ParentNotOverCountedOnLocalRefuse is the regression test for
// the over-count bug: when many sibling callers pass the local check, all
// record a slot in the shared parent, but only one wins the local re-verify,
// the parent must reflect exactly the number of operations that were ALLOWED —
// not the number that briefly took a parent slot. The child limit is 1 and the
// window never expires during the test, so exactly one call may succeed; the
// shared parent count must equal that one success, never more.
//
// The assertion (parent count == allowed) is an invariant that always holds
// once the slot is released; the bug violates it whenever the race fires, and
// the concurrent fan-out makes it fire reliably. Run under -race.
func TestRateLimiter_ParentNotOverCountedOnLocalRefuse(t *testing.T) {
	for iter := range 50 {
		parent := NewRateLimiter(1_000_000, time.Minute) // never the binding limit
		child := NewRateLimiter(1, time.Minute)          // admits exactly one op
		child.SetParent(parent)

		const goroutines = 64
		start := make(chan struct{})
		var wg sync.WaitGroup
		var allowed atomic.Int64
		wg.Add(goroutines)
		for range goroutines {
			go func() {
				defer wg.Done()
				<-start // align so the callers race through the first check together
				if child.Allow() {
					allowed.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()

		got := allowed.Load()
		if got != 1 {
			t.Fatalf("iter %d: child limit 1 should admit exactly 1 op, got %d", iter, got)
		}
		if parentCount, _, _ := parent.Snapshot(); int64(parentCount) != got {
			t.Fatalf("iter %d: parent over-counted — recorded %d slots for %d allowed op(s)",
				iter, parentCount, got)
		}
	}
}
