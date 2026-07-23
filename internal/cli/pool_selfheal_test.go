package cli

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
)

// selfHealPool builds a bare pool with a short first-start grace, for driving
// awaitReady directly with a hand-controlled readyCh. No real language-server
// process is spawned: the tests here exercise the late-failure drain in
// isolation, so a nil supervisor/cache on the entry is fine (removeFailed guards
// both). A short startGrace makes the grace branch fire deterministically
// instead of after the 2 s production window.
func selfHealPool(startGrace time.Duration) *workspacePool {
	return &workspacePool{
		entries:    make(map[poolKey]*poolEntry),
		baseCtx:    context.Background(),
		cacheTTL:   time.Minute,
		startGrace: startGrace,
	}
}

// insertWarming publishes a not-yet-ready entry into the pool the way
// startOrReuse would, and returns it.
func insertWarming(p *workspacePool, root string) *poolEntry {
	e := &poolEntry{root: root, language: "go", proxy: &clientProxy{}, state: poolActive, startedAt: time.Now()}
	p.mu.Lock()
	p.entries[poolKey{root, "go"}] = e
	p.mu.Unlock()
	return e
}

// TestAwaitReady_SlowFailureEvictsAndSelfHeals is the core regression test: a
// first start that fails AFTER the grace window (nobody left reading readyCh in
// the old code) must still evict the dead entry, and the NEXT acquire must build
// a fresh one. Without the drain the dead entry (proxy.get() == nil) was reused
// forever with no self-heal.
func TestAwaitReady_SlowFailureEvictsAndSelfHeals(t *testing.T) {
	cmd, args := sleepCommand(t)
	pool := warmingPool(context.Background(), cmd, args)
	pool.startGrace = 15 * time.Millisecond
	defer pool.close()

	const root = "/tmp/plumb-slowfail-selfheal-root"
	dead := insertWarming(pool, root)
	readyCh := make(chan error, 1)

	// awaitReady bails at the grace with the not-yet-ready entry and hands
	// readyCh to the drain goroutine.
	got, err := pool.awaitReady(context.Background(), dead, readyCh)
	if err != nil {
		t.Fatalf("awaitReady returned an error at the grace: %v", err)
	}
	if got != dead {
		t.Fatal("awaitReady did not return the warming entry at the grace")
	}
	if pool.lookup(root, "go") != dead {
		t.Fatal("entry vanished before the late failure arrived")
	}

	// The first start now fails, slowly — after awaitReady already returned.
	readyCh <- errors.New("initialize: connection closed")

	// Self-heal 1: the drain evicts the dead entry.
	waitEntryGone(t, pool, root, time.Second)

	// Self-heal 2: the next acquire builds a FRESH entry instead of reusing the
	// dead one — proving the pool recovered rather than caching a corpse.
	fresh, err := pool.acquireLang(context.Background(), root, "go", false)
	if err != nil {
		t.Fatalf("acquire after eviction: %v", err)
	}
	if fresh == nil {
		t.Fatal("acquire after eviction returned nil entry")
	}
	if fresh == dead {
		t.Fatal("acquire after eviction reused the dead entry; self-heal failed")
	}
}

// TestAwaitReady_SlowFailureViaCancelledCtx covers the second bail-out path: a
// cancelled request context returns the warming entry immediately (the
// supervisor keeps warming on the pool's base ctx), and a later failure must
// still be drained and evicted — the same leak applied to this branch.
func TestAwaitReady_SlowFailureViaCancelledCtx(t *testing.T) {
	pool := selfHealPool(time.Hour) // grace is irrelevant; the ctx branch wins
	const root = "/tmp/plumb-slowfail-cancelctx-root"
	e := insertWarming(pool, root)
	readyCh := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // request already gone before the call

	got, err := pool.awaitReady(ctx, e, readyCh)
	if err != nil {
		t.Fatalf("awaitReady on a cancelled ctx returned an error: %v", err)
	}
	if got != e {
		t.Fatal("awaitReady did not return the warming entry on ctx cancel")
	}

	readyCh <- errors.New("initialize failed after the request was cancelled")
	waitEntryGone(t, pool, root, time.Second)
}

// TestAwaitReady_SlowSuccessNotEvicted is the critical false-positive guard: an
// entry that becomes ready AFTER the grace (the common cold-start-of-a-large-
// workspace case) must NOT be evicted by the drain. A nil outcome is a no-op.
func TestAwaitReady_SlowSuccessNotEvicted(t *testing.T) {
	pool := selfHealPool(15 * time.Millisecond)
	const root = "/tmp/plumb-slowsuccess-root"
	e := insertWarming(pool, root)
	readyCh := make(chan error, 1)

	if _, err := pool.awaitReady(context.Background(), e, readyCh); err != nil {
		t.Fatalf("awaitReady at the grace: %v", err)
	}

	// The server became ready after the grace: a successful late outcome.
	readyCh <- nil

	// Give the drain ample time to (wrongly) act, then assert the entry survives.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if pool.lookup(root, "go") != e {
			t.Fatal("a healthy slow-start entry was wrongly evicted by the drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAwaitReady_LateFailureLeavesOtherEntriesAlone pins per-entry isolation: a
// late failure evicts only its own entry, never a sibling that is warming or
// ready under a different root. removeFailed's map-identity guard is what makes
// this safe.
func TestAwaitReady_LateFailureLeavesOtherEntriesAlone(t *testing.T) {
	pool := selfHealPool(15 * time.Millisecond)
	const (
		rootFail = "/tmp/plumb-iso-fail-root"
		rootLive = "/tmp/plumb-iso-live-root"
	)
	eFail := insertWarming(pool, rootFail)
	eLive := insertWarming(pool, rootLive)

	readyFail := make(chan error, 1)
	readyLive := make(chan error, 1)

	if _, err := pool.awaitReady(context.Background(), eFail, readyFail); err != nil {
		t.Fatalf("awaitReady(fail): %v", err)
	}
	if _, err := pool.awaitReady(context.Background(), eLive, readyLive); err != nil {
		t.Fatalf("awaitReady(live): %v", err)
	}

	readyFail <- errors.New("boom")
	readyLive <- nil // the sibling succeeds

	waitEntryGone(t, pool, rootFail, time.Second)

	if pool.lookup(rootLive, "go") != eLive {
		t.Fatal("evicting the failed entry also removed an unrelated sibling entry")
	}
}

// TestAwaitReady_DrainNoGoroutineLeak proves every drain goroutine terminates —
// whether the late outcome is a failure (evict then return) or a success
// (return immediately). Run under -race for the concurrency guarantee; here we
// assert the live goroutine count settles back to its baseline.
func TestAwaitReady_DrainNoGoroutineLeak(t *testing.T) {
	pool := selfHealPool(5 * time.Millisecond)

	// Let any goroutines from earlier setup drain first, then sample a baseline.
	time.Sleep(20 * time.Millisecond)
	base := runtime.NumGoroutine()

	const n = 30
	for i := 0; i < n; i++ {
		root := fmt.Sprintf("/tmp/plumb-drain-leak-%d", i)
		e := insertWarming(pool, root)
		readyCh := make(chan error, 1)
		if _, err := pool.awaitReady(context.Background(), e, readyCh); err != nil {
			t.Fatalf("awaitReady[%d]: %v", i, err)
		}
		if i%2 == 0 {
			readyCh <- nil // late success
		} else {
			readyCh <- errors.New("late failure") // late failure
		}
	}

	// Poll until the drains have all exited (count returns near the baseline).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := runtime.NumGoroutine(); got <= base+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count %d did not settle to baseline %d(+2); drains leaked",
				runtime.NumGoroutine(), base)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
