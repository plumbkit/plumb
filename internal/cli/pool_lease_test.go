package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
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
	victim := pool.overBudgetVictimLocked("go", 2, nil)
	pool.mu.Unlock()
	if victim != newer {
		t.Fatalf("victim = %v, want the idle newer entry, not the busy LRU one", victim)
	}
	_, doneNew := newer.proxy.begin()
	pool.mu.Lock()
	victim = pool.overBudgetVictimLocked("go", 2, nil)
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

// TestPool_GraceCountsFromTheLastCall: a call made during the grace that follows
// the last release cancels that teardown, so the server stays for a full grace
// after the call ends — not just until the grace armed by the release expires.
func TestPool_GraceCountsFromTheLastCall(t *testing.T) {
	t.Run("call started after the release", func(t *testing.T) { graceFromLastCall(t, false) })
	t.Run("call started before the release", func(t *testing.T) { graceFromLastCall(t, true) })
}

func graceFromLastCall(t *testing.T, callFirst bool) {
	t.Helper()
	pool, ctx := fastWarmingPool(t, 600*time.Millisecond)
	defer pool.close()
	const root = "/tmp/plumb-lease-last-call"

	if _, err := pool.acquireLang(ctx, root, "go", true); err != nil {
		t.Fatalf("pinned acquire: %v", err)
	}
	var done func()
	call := func() {
		var err error
		if _, done, err = pool.acquireCall(ctx, root, "go"); err != nil {
			t.Fatalf("acquireCall: %v", err)
		}
	}
	if callFirst {
		call()
	}
	released := time.Now()
	pool.release(root, "go") // arms a teardown for released+grace
	if !callFirst {
		call()
	}
	time.Sleep(400 * time.Millisecond)
	done() // the call ends inside the release's grace; its own runs to ~1000 ms
	// Look between the two: the release's grace has expired (600 ms), the call's
	// has not, with 200 ms of margin on each side.
	time.Sleep(time.Until(released.Add(pool.idleGrace + 200*time.Millisecond)))
	if pool.lookup(root, "go") == nil {
		t.Fatal("reaped when the release's grace expired, although a call ended after the release")
	}
	waitEntryGone(t, pool, root, 2*time.Second)
}

// TestClientProxy_BeginOnANotReadyServerTakesNoLease: begin on a server that is
// not ready returns nothing and leaves no call counted, so it cannot block a
// later hibernation or reap.
func TestClientProxy_BeginOnANotReadyServerTakesNoLease(t *testing.T) {
	p := &clientProxy{}
	if c, done := p.begin(); c != nil || done != nil {
		t.Fatal("begin on a not-ready proxy returned a client")
	}
	if n := p.inflight.Load(); n != 0 {
		t.Fatalf("inflight = %d after a not-ready begin, want 0", n)
	}
}

// TestRoutingProxy_PrimaryFastPathHoldsACallLease: the primary's lock-free
// path — both a URI-less call and a routed one landing on the primary — holds a
// call lease until the caller's done, so hibernation waits for it.
func TestRoutingProxy_PrimaryFastPathHoldsACallLease(t *testing.T) {
	root := goModRoot(t)
	for _, tc := range []struct {
		name string
		get  func(*routingProxy) (func(), error)
	}{
		{"primaryClient", func(rp *routingProxy) (func(), error) {
			_, done, err := rp.primaryClient(context.Background())
			return done, err
		}},
		{"route to the primary", func(rp *routingProxy) (func(), error) {
			_, done, err := rp.route(context.Background(), protocol.FileURI(filepath.Join(root, "main.go")), true)
			return done, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool()
			e := leasedEntry(t, pool, root, &shutdownProbe{})
			rp := newRoutingProxy(pool)
			rp.setPrimary(root, "go", e.proxy)

			done, err := tc.get(rp)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			pool.hibernateEntry(e)
			if got := entryState(pool, e); got != poolActive {
				t.Fatalf("state = %v while the primary call held its lease, want active", got)
			}
			done()
			pool.hibernateEntry(e)
			if got := entryState(pool, e); got != poolHibernated {
				t.Fatalf("state = %v after the call ended, want hibernated", got)
			}
		})
	}
}

// TestPool_OnStartMarksTheEntryActive drives the pool's OnStart hook through a
// real initialize handshake against an in-process fake server: a starting entry
// becomes active once the handshake succeeds.
func TestPool_OnStartMarksTheEntryActive(t *testing.T) {
	pool := newTestPool()
	const root = "/tmp/plumb-lease-onstart"
	e := &poolEntry{root: root, language: "go", proxy: &clientProxy{}, inv: cache.NewInvalidator(cache.New(time.Minute, 100)), state: poolStarting}
	pool.entries[poolKey{root, "go"}] = e

	toServerR, toServerW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	t.Cleanup(func() {
		_ = toServerW.Close()
		_ = toClientW.Close()
	})
	server := jsonrpc.NewConn(toServerR, toClientW)
	server.SetRequestHandler(func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "initialize" {
			return map[string]any{"capabilities": map[string]any{}}, nil
		}
		return nil, nil
	})
	client := jsonrpc.NewConn(toClientR, toServerW)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.poolOnStart(e, protocol.FileURI(root), config.LSPConfig{})(ctx, client); err != nil {
		t.Fatalf("OnStart: %v", err)
	}
	if got := entryState(pool, e); got != poolActive {
		t.Fatalf("state = %v after a successful handshake, want active", got)
	}
}

// TestRestartSwift_WaitsForACallInProgress: an explicit restart that meets a
// call holding its lease waits for the call to end, then restarts, instead of
// failing with "entry did not hibernate" (the #683 review's regression).
func TestRestartSwift_WaitsForACallInProgress(t *testing.T) {
	pool := &workspacePool{entries: make(map[poolKey]*poolEntry), baseCtx: context.Background(), cacheTTL: time.Minute, startGrace: 15 * time.Millisecond}
	defer pool.close()
	root := t.TempDir()
	release := make(chan struct{})
	close(release) // the woken start succeeds at once
	var outcome error
	e := swiftEntryWithControllableStart(t, pool, root, nil, release, &outcome)
	e.proxy.set(&stubClient{id: "swift"})

	_, done := e.proxy.begin()
	ended := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		done()
		close(ended)
	}()
	if err := runRestartSwift(t, pool, root, 5*time.Second); err != nil {
		t.Fatalf("restartSwift with a call in progress: %v", err)
	}
	select {
	case <-ended:
	default:
		t.Fatal("restartSwift returned before the call in progress ended")
	}
}

// TestPool_AbortedReapKeepsTheClientAndRearms: a reap that fires while a call
// holds the entry leaves it fully usable — the live client is restored, not left
// cleared — and the call's end re-arms the teardown even though that reap had
// already fired its timer.
func TestPool_AbortedReapKeepsTheClientAndRearms(t *testing.T) {
	pool := newTestPool()
	pool.idleGrace = 30 * time.Millisecond
	probe := &shutdownProbe{}
	e := leasedEntry(t, pool, "/tmp/plumb-lease-abort", probe)
	e.proxy.onIdle = func() { go pool.scheduleIdleReap(e) }

	c, done := e.proxy.begin()
	if c == nil {
		t.Fatal("begin on a ready entry returned no client")
	}
	pool.mu.Lock()
	e.graceTimer = time.AfterFunc(time.Hour, func() {}) // as a release would arm it
	pool.mu.Unlock()
	pool.reapEntry(e) // the timer firing mid-call
	if e.proxy.get() == nil {
		t.Fatal("an aborted reap left the live client cleared")
	}
	if probe.shutdown.Load() != 0 {
		t.Fatal("an aborted reap shut the server down")
	}
	done()
	waitEntryGone(t, pool, "/tmp/plumb-lease-abort", 2*time.Second)
}

// TestClientProxy_NotReadyBeginReArmsAnUnheldEntry: a not-ready begin on an
// entry nobody holds returns its count through the same idle check as a call's
// end, so a reap it made stand down is re-armed.
func TestClientProxy_NotReadyBeginReArmsAnUnheldEntry(t *testing.T) {
	var idle atomic.Int32
	p := &clientProxy{onIdle: func() { idle.Add(1) }}
	if c, _ := p.begin(); c != nil {
		t.Fatal("not-ready begin returned a client")
	}
	if idle.Load() != 1 {
		t.Fatalf("onIdle ran %d times after a not-ready begin on an unheld entry, want 1", idle.Load())
	}
	p.held.Store(true)
	p.begin()
	if idle.Load() != 1 {
		t.Fatal("onIdle ran for a held entry")
	}
}

// TestRestartSwift_WaitStopsAtShutdown: the restart's wait for calls in
// progress ends at once when the daemon shuts down, rather than sleeping out
// its 30 s bound.
func TestRestartSwift_WaitStopsAtShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pool := &workspacePool{entries: make(map[poolKey]*poolEntry), baseCtx: ctx, cacheTTL: time.Minute, startGrace: 15 * time.Millisecond}
	defer pool.close()
	root := t.TempDir()
	release := make(chan struct{})
	close(release)
	var outcome error
	e := swiftEntryWithControllableStart(t, pool, root, nil, release, &outcome)
	e.proxy.set(&stubClient{id: "swift"})
	_, done := e.proxy.begin()
	defer done()
	cancel()
	err := runRestartSwift(t, pool, root, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("restart during shutdown = %v, want the shutting-down error", err)
	}
}

// TestPool_ZeroGraceMeansTheDefault: a pool built without an idle grace does not
// tear an unpinned entry down the moment its call ends (the shape of every
// struct-literal test pool, and the flake TestWorkspacePoolClose_PromptNoWarn
// hit once leases made unpinned entries reapable).
func TestPool_ZeroGraceMeansTheDefault(t *testing.T) {
	cmd, args := sleepCommand(t)
	pool := warmingPool(context.Background(), cmd, args) // idleGrace left zero
	pool.startGrace = 20 * time.Millisecond
	defer pool.close()
	root := t.TempDir()
	if _, err := pool.acquireLang(context.Background(), root, "go", false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if pool.lookup(root, "go") == nil {
		t.Fatal("a zero-grace pool tore the entry down at once")
	}
	if g := pool.grace(); g != poolIdleGrace {
		t.Fatalf("grace = %s, want the default %s", g, poolIdleGrace)
	}
}
