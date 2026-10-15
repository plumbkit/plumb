package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// These tests pin PLAN-533's lease model: the pool owns each language-server
// entry; sessions hold session leases (pins and borrows) and calls hold call
// leases; an entry is hibernated or reaped only when no call is running, and
// reaped only once no session lease is left either.

// leasedEntry installs a ready stub entry and returns it.
func leasedEntry(t *testing.T, pool *workspacePool, root string, c *shutdownProbe) *poolEntry {
	t.Helper()
	installEntry(pool, root, c)
	return pool.entries[poolKey{root, "go"}]
}

// shutdownProbe is a stub client that records a Shutdown arriving while a call
// is still using it: the one thing a call lease exists to prevent.
type shutdownProbe struct {
	stubClient
	inUse    atomic.Int32
	violated atomic.Bool
	shutdown atomic.Int32
}

func (s *shutdownProbe) Shutdown(context.Context) error {
	s.shutdown.Add(1)
	if s.inUse.Load() > 0 {
		s.violated.Store(true)
	}
	return nil
}

func entryState(pool *workspacePool, e *poolEntry) poolLifecycle {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return e.state
}

// TestPool_CallLeaseBlocksHibernation: hibernating an entry with a call in
// progress is refused — the server keeps running and the caller's client stays
// live — and succeeds once the call has ended.
func TestPool_CallLeaseBlocksHibernation(t *testing.T) {
	pool := newTestPool()
	probe := &shutdownProbe{}
	e := leasedEntry(t, pool, "/tmp/plumb-lease-hibernate", probe)

	c, done := e.proxy.begin()
	if c == nil {
		t.Fatal("begin on a ready entry returned no client")
	}
	pool.hibernateEntry(e)
	if got := entryState(pool, e); got != poolActive {
		t.Fatalf("state = %v during a call, want active", got)
	}
	if e.proxy.get() == nil || probe.shutdown.Load() != 0 {
		t.Fatal("hibernation stopped the server while a call held it")
	}
	done()
	pool.hibernateEntry(e)
	if got := entryState(pool, e); got != poolHibernated {
		t.Fatalf("state = %v after the call ended, want hibernated", got)
	}
}

// TestPool_CallLeaseVersusHibernation_Race runs a lock-free call (the primary
// fast path) against hibernation many times: a call that got a client must
// never see that client shut down under it. Run under -race.
func TestPool_CallLeaseVersusHibernation_Race(t *testing.T) {
	pool := newTestPool()
	probe := &shutdownProbe{}
	e := leasedEntry(t, pool, "/tmp/plumb-lease-race", probe)

	for i := 0; i < 3000 && !probe.violated.Load(); i++ {
		pool.mu.Lock()
		e.state = poolActive
		pool.mu.Unlock()
		e.proxy.set(probe)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if c, done := e.proxy.begin(); c != nil {
				probe.inUse.Add(1)
				runtime.Gosched()
				probe.inUse.Add(-1)
				done()
			}
		}()
		go func() {
			defer wg.Done()
			pool.hibernateEntry(e)
		}()
		wg.Wait()
	}
	if probe.violated.Load() {
		t.Fatal("a server was shut down while a call that obtained it was still running")
	}
}

// TestPool_CallLeaseBlocksReapAndRearmsIt: an unheld entry whose idle teardown
// fires during a call stays, and the call's end re-arms the teardown, so the
// entry is still reclaimed once idle.
func TestPool_CallLeaseBlocksReapAndRearmsIt(t *testing.T) {
	pool, ctx := fastWarmingPool(t, 30*time.Millisecond)
	defer pool.close()
	const root = "/tmp/plumb-lease-reap"

	e, done, err := pool.acquireCall(ctx, root, "go")
	if err != nil {
		t.Fatalf("acquireCall: %v", err)
	}
	pool.reapEntry(e) // the timer firing mid-call
	if pool.lookup(root, "go") != e {
		t.Fatal("reap removed an entry a call still held")
	}
	time.Sleep(3 * pool.idleGrace)
	if pool.lookup(root, "go") != e {
		t.Fatal("entry reclaimed while its call was still running")
	}
	done()
	waitEntryGone(t, pool, root, 2*time.Second)
}

// TestPool_CallCancelsPendingTeardown: a call on an entry whose last session
// lease has gone cancels the armed teardown, so the grace counts from the end of
// the last call, not from the release.
func TestPool_CallCancelsPendingTeardown(t *testing.T) {
	pool, ctx := fastWarmingPool(t, 40*time.Millisecond)
	defer pool.close()
	const root = "/tmp/plumb-lease-cancel"

	if _, err := pool.acquireLang(ctx, root, "go", true); err != nil {
		t.Fatalf("pinned acquire: %v", err)
	}
	pool.release(root, "go") // arms the teardown
	_, done, err := pool.acquireCall(ctx, root, "go")
	if err != nil {
		t.Fatalf("acquireCall: %v", err)
	}
	time.Sleep(3 * pool.idleGrace)
	if pool.lookup(root, "go") == nil {
		t.Fatal("the teardown armed by release fired during a later call")
	}
	done()
	waitEntryGone(t, pool, root, 2*time.Second)
}

// goModRoot makes a directory the pool detects as a Go root.
func goModRoot(t *testing.T) string {
	t.Helper()
	dir := freshTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module lease\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestRoutingProxy_RoutedEntryBorrowedUntilReleased: a routed call borrows its
// entry for the connection — the entry outlives the call and the grace — and
// returning the borrow lets the pool reap it. Before PLAN-533 a routed entry
// lived until daemon shutdown.
func TestRoutingProxy_RoutedEntryBorrowedUntilReleased(t *testing.T) {
	pool, _ := fastWarmingPool(t, 30*time.Millisecond)
	defer pool.close()
	root := goModRoot(t)
	rp := newRoutingProxy(pool)
	rp.warmCap = time.Millisecond

	if _, done, err := rp.route(cancelledCtx(), protocol.FileURI(filepath.Join(root, "main.go")), true); err == nil {
		done()
		t.Fatal("route to a never-ready server returned a client")
	}
	if got := poolRefs(pool, root); got != 1 {
		t.Fatalf("refs = %d after a routed call, want 1 (the connection's borrow)", got)
	}
	time.Sleep(3 * pool.idleGrace)
	if pool.lookup(root, "go") == nil {
		t.Fatal("a borrowed routed entry was reaped while its connection lived")
	}
	rp.releaseBorrowed()
	waitEntryGone(t, pool, root, 2*time.Second)
}

// TestRoutingProxy_NestedBorrowersShareOneEntry: two connections borrowing the
// same entry (a nested-root view and a direct one) hold two leases; the entry
// survives the first release and goes after the second.
func TestRoutingProxy_NestedBorrowersShareOneEntry(t *testing.T) {
	pool, _ := fastWarmingPool(t, 30*time.Millisecond)
	defer pool.close()
	root := goModRoot(t)
	uri := protocol.FileURI(filepath.Join(root, "main.go"))
	a, b := newRoutingProxy(pool), newRoutingProxy(pool)
	a.warmCap, b.warmCap = time.Millisecond, time.Millisecond

	for _, rp := range []*routingProxy{a, b, a} { // a twice: one borrow per connection
		_, _, _ = rp.route(cancelledCtx(), uri, true)
	}
	if got := poolRefs(pool, root); got != 2 {
		t.Fatalf("refs = %d, want 2 (one borrow per connection)", got)
	}
	a.releaseBorrowed()
	time.Sleep(3 * pool.idleGrace)
	if pool.lookup(root, "go") == nil {
		t.Fatal("entry reaped while another connection still borrowed it")
	}
	b.releaseBorrowed()
	waitEntryGone(t, pool, root, 2*time.Second)
}

// TestRoutingProxy_FanOutBorrowsAndEndsItsCall: a WorkspaceSymbols fan-out
// target is borrowed like a routed one, and its call lease ends with the query.
func TestRoutingProxy_FanOutBorrowsAndEndsItsCall(t *testing.T) {
	pool, _ := fastWarmingPool(t, 30*time.Millisecond)
	defer pool.close()
	root := goModRoot(t)
	rp := newRoutingProxy(pool)
	rp.warmCap = time.Millisecond

	ctx := cancelledCtx()
	if _, _, err := rp.symbolsFrom(ctx, ctx, lsTarget{root: root, language: "go"}, protocol.WorkspaceSymbolParams{}); err != nil {
		t.Fatalf("symbolsFrom: %v", err)
	}
	e := pool.lookup(root, "go")
	if e == nil {
		t.Fatal("fan-out target missing")
	}
	if got := poolRefs(pool, root); got != 1 {
		t.Fatalf("refs = %d after a fan-out, want 1 (the connection's borrow)", got)
	}
	if n := e.proxy.inflight.Load(); n != 0 {
		t.Fatalf("inflight = %d after the fan-out returned, want 0", n)
	}
}

// TestConnSession_CloseReturnsBorrowedLeases: ending a connection returns the
// leases its routing proxy borrowed, so the routed server is reaped.
func TestConnSession_CloseReturnsBorrowedLeases(t *testing.T) {
	pool, _ := fastWarmingPool(t, 30*time.Millisecond)
	defer pool.close()
	store, ss := newOriginStore(t)
	s := newConnSession(context.Background(), pool, nil, store, nil, ss, newSharedBudgets())
	root := goModRoot(t)
	s.sessionProxy.warmCap = time.Millisecond

	_, _, _ = s.sessionProxy.route(cancelledCtx(), protocol.FileURI(filepath.Join(root, "main.go")), true)
	if got := poolRefs(pool, root); got != 1 {
		t.Fatalf("refs = %d after a routed call, want 1", got)
	}
	s.close()
	waitEntryGone(t, pool, root, 2*time.Second)
}

// TestRoutingProxy_ResetPrimaryReturnsBorrowedLeases: a deliberate workspace
// switch starts the connection with no borrowed entries.
func TestRoutingProxy_ResetPrimaryReturnsBorrowedLeases(t *testing.T) {
	pool, _ := fastWarmingPool(t, 30*time.Millisecond)
	defer pool.close()
	root := goModRoot(t)
	rp := newRoutingProxy(pool)
	rp.warmCap = time.Millisecond

	_, _, _ = rp.route(cancelledCtx(), protocol.FileURI(filepath.Join(root, "main.go")), true)
	if got := poolRefs(pool, root); got != 1 {
		t.Fatalf("refs = %d after a routed call, want 1", got)
	}
	rp.resetPrimary("/tmp/plumb-lease-elsewhere", "go", &clientProxy{})
	waitEntryGone(t, pool, root, 2*time.Second)
}

// TestPool_OverBudgetSkipsABusyEntry: the max_workspaces victim is never an
// entry with a call in progress; with every entry busy there is no victim.
func TestPool_OverBudgetSkipsABusyEntry(t *testing.T) {
	pool := newTestPool()
	older := leasedEntry(t, pool, "/tmp/plumb-budget-old", &shutdownProbe{})
	newer := leasedEntry(t, pool, "/tmp/plumb-budget-new", &shutdownProbe{})
	older.proxy.lastUsed.Store(1)
	newer.proxy.lastUsed.Store(2)

	_, doneOld := older.proxy.begin()
	pool.mu.Lock()
	victim := pool.overBudgetVictimLocked("go", 2)
	pool.mu.Unlock()
	if victim != newer {
		t.Fatalf("victim = %v, want the idle newer entry, not the busy LRU one", victim)
	}
	_, doneNew := newer.proxy.begin()
	pool.mu.Lock()
	victim = pool.overBudgetVictimLocked("go", 2)
	pool.mu.Unlock()
	if victim != nil {
		t.Fatalf("victim = %s with every entry busy, want none", victim.root)
	}
	doneOld()
	doneNew()
}

// TestPool_EntryStates: a new entry is starting until its handshake completes;
// markActive is the only step to active, and it never revives a hibernated entry.
func TestPool_EntryStates(t *testing.T) {
	pool, ctx := fastWarmingPool(t, time.Minute)
	defer pool.close()
	const root = "/tmp/plumb-lease-states"
	e, err := pool.acquireLang(ctx, root, "go", true)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := entryState(pool, e); got != poolStarting {
		t.Fatalf("state = %v before the handshake, want starting", got)
	}
	pool.markActive(e)
	if got := entryState(pool, e); got != poolActive {
		t.Fatalf("state = %v after markActive, want active", got)
	}
	pool.hibernateEntry(e)
	pool.markActive(e)
	if got := entryState(pool, e); got != poolHibernated {
		t.Fatalf("state = %v: markActive revived a hibernated entry", got)
	}
}
