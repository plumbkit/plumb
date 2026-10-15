package cli

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/topology"
)

// The edges of PLAN-536's unlocked opens, from #689's review: an open that
// finishes after shutdown, or after a sweep, must not publish; a panicking open
// must not wedge its key; a waiter re-checks what the leader published.

// watcherStopped reports whether w has been stopped.
func watcherStopped(w *lspFSWatcher) bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// TestPool_AttachWatcherAfterCloseStopsIt: a watcher whose build finishes after
// close() swept the pool is stopped, not attached — close() keeps the entries
// in the map, so only the closed flag can tell.
func TestPool_AttachWatcherAfterCloseStopsIt(t *testing.T) {
	pool, ctx := fastWarmingPool(t, time.Minute)
	pool.watcherFactory = func(string, *clientProxy) (*lspFSWatcher, error) { return nil, errSeam }
	root := goModRoot(t)
	e, err := pool.acquireLang(ctx, root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := newLSPFSWatcher(root, e.proxy)
	if err != nil {
		t.Skipf("no file watcher on this platform: %v", err)
	}
	pool.close()
	pool.watcherFactory = func(string, *clientProxy) (*lspFSWatcher, error) { return w, nil }
	pool.attachWatcher(e)
	pool.mu.Lock()
	attached := e.watcher
	pool.mu.Unlock()
	if attached != nil {
		t.Fatal("a watcher was attached to an entry close() had already swept")
	}
	if !watcherStopped(w) {
		t.Fatal("the unattached watcher was left running")
	}
}

// TestRestartSwift_RebuildsTheWatcher: a Swift restart wakes the entry and
// rebuilds its file watcher (hibernation dropped it).
func TestRestartSwift_RebuildsTheWatcher(t *testing.T) {
	pool := &workspacePool{
		entries:    make(map[poolKey]*poolEntry),
		baseCtx:    t.Context(),
		cacheTTL:   time.Minute,
		startGrace: time.Second,
	}
	defer pool.close()
	var built atomic.Int32
	pool.watcherFactory = func(string, *clientProxy) (*lspFSWatcher, error) {
		built.Add(1)
		return nil, errSeam
	}
	root := t.TempDir()
	release := make(chan struct{})
	close(release)
	var outcome error
	swiftEntryWithControllableStart(t, pool, root, nil, release, &outcome)
	if err := runRestartSwift(t, pool, root, 5*time.Second); err != nil {
		t.Fatalf("restartSwift: %v", err)
	}
	if built.Load() == 0 {
		t.Fatal("restartSwift did not rebuild the file watcher")
	}
}

// panicsOnce runs fn and swallows its panic, as config.notifyListener does for
// the daemon's reload path.
func panicsOnce(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// TestPools_APanickingOpenDoesNotWedgeTheKey: after an open panics, the next
// acquire of the same key opens again instead of waiting forever on a flight
// nobody ends — for each of the three stores.
func TestPools_APanickingOpenDoesNotWedgeTheKey(t *testing.T) {
	t.Run("topology", func(t *testing.T) {
		p := newTopologyPool(config.TopologyConfig{Enabled: true})
		defer p.StopAll()
		var calls atomic.Int32
		p.openStore = func(root string, cfg config.TopologyConfig) (*topology.Store, error) {
			if calls.Add(1) == 1 {
				panic("seam: open panicked")
			}
			return topology.Open(root, cfg, nil)
		}
		root, cfg := t.TempDir(), config.TopologyConfig{Enabled: true}
		panicsOnce(func() { p.Acquire(root, cfg) })
		within(t, 2*time.Second, "the Acquire after a panicking open", func() {
			if p.Acquire(root, cfg) == nil {
				t.Error("the re-open failed")
			}
		})
	})
	t.Run("memory", func(t *testing.T) {
		p := newMemoryIndexPool()
		defer p.CloseAll()
		var calls atomic.Int32
		p.openIndex = func(ws string) (*memory.Index, error) {
			if calls.Add(1) == 1 {
				panic("seam: open panicked")
			}
			return memory.OpenIndex(ws)
		}
		ws := t.TempDir()
		panicsOnce(func() { p.Acquire(ws) })
		within(t, 2*time.Second, "the Acquire after a panicking open", func() {
			if p.Acquire(ws) == nil {
				t.Error("the re-open failed")
			}
		})
	})
	t.Run("collab", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		p := newCollabPool()
		defer p.closeAll()
		var calls atomic.Int32
		p.openStore = func(ws string, _ bool) (*collab.Store, error) {
			if calls.Add(1) == 1 {
				panic("seam: open panicked")
			}
			return collab.Open(ws)
		}
		ws := t.TempDir()
		panicsOnce(func() { p.acquire(ws) })
		within(t, 2*time.Second, "the acquire after a panicking open", func() {
			if p.acquire(ws) == nil {
				t.Error("the re-open failed")
			}
		})
	})
}

// blockingTopologyOpen makes p's open of any root signal entered and wait for
// release, counting opens.
func blockingTopologyOpen(p *topologyPool, entered chan<- struct{}, release <-chan struct{}, opens *atomic.Int32) {
	var once sync.Once
	p.openStore = func(root string, cfg config.TopologyConfig) (*topology.Store, error) {
		if opens.Add(1) == 1 {
			once.Do(func() { close(entered) })
			<-release
		}
		return topology.Open(root, cfg, nil)
	}
}

// TestTopologyPool_NoPublishAfterStopOrSweep: an open still running when the
// pool is stopped, or when a reload disables topology, does not publish its
// store.
func TestTopologyPool_NoPublishAfterStopOrSweep(t *testing.T) {
	cfg := config.TopologyConfig{Enabled: true}
	for name, interrupt := range map[string]func(p *topologyPool){
		"stopped":  func(p *topologyPool) { p.StopAll() },
		"disabled": func(p *topologyPool) { p.Reconcile(config.TopologyConfig{Enabled: false}) },
	} {
		t.Run(name, func(t *testing.T) {
			p := newTopologyPool(cfg)
			defer p.StopAll()
			entered, release := make(chan struct{}), make(chan struct{})
			var opens atomic.Int32
			blockingTopologyOpen(p, entered, release, &opens)
			got := make(chan *topology.Store, 1)
			go func() { got <- p.Acquire(t.TempDir(), cfg) }()
			<-entered
			interrupt(p)
			close(release)
			if s := <-got; s != nil {
				t.Fatal("an open that outlived the interruption was handed out")
			}
			p.mu.Lock()
			n := len(p.stores)
			p.mu.Unlock()
			if n != 0 {
				t.Fatalf("%d store(s) published after the interruption", n)
			}
		})
	}
}

// TestTopologyPool_WaiterWithAnotherConfigReopens: a waiter whose config
// differs from the leader's does not take the leader's store; it re-opens with
// its own.
func TestTopologyPool_WaiterWithAnotherConfigReopens(t *testing.T) {
	p := newTopologyPool(config.TopologyConfig{Enabled: true})
	defer p.StopAll()
	entered, release := make(chan struct{}), make(chan struct{})
	var opens atomic.Int32
	blockingTopologyOpen(p, entered, release, &opens)
	root := t.TempDir()
	leaderCfg := config.TopologyConfig{Enabled: true}
	waiterCfg := config.TopologyConfig{Enabled: true, ResyncIntervalMinutes: 9}
	go p.Acquire(root, leaderCfg)
	<-entered
	got := make(chan *topology.Store, 1)
	go func() { got <- p.Acquire(root, waiterCfg) }()
	time.Sleep(50 * time.Millisecond) // let the waiter reach the flight
	close(release)
	if <-got == nil {
		t.Fatal("the waiter's Acquire failed")
	}
	p.mu.Lock()
	published := p.cfgs[root]
	p.mu.Unlock()
	if opens.Load() != 2 || published.ResyncIntervalMinutes != 9 {
		t.Fatalf("opens = %d, published config %+v; want the waiter's own re-open", opens.Load(), published)
	}
}

// TestCollabPool_CoalescedAndClosedAfterShutdown: two acquires of one store
// share a single open; an open that finishes after closeAll is closed, not
// published.
func TestCollabPool_CoalescedAndClosedAfterShutdown(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	p := newCollabPool()
	ws := t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	var opens atomic.Int32
	p.openStore = func(ws string, _ bool) (*collab.Store, error) {
		if opens.Add(1) == 1 {
			close(entered)
			<-release
		}
		return collab.Open(ws)
	}
	first, second := make(chan *collab.Store, 1), make(chan *collab.Store, 1)
	go func() { first <- p.acquire(ws) }()
	<-entered
	go func() { second <- p.acquire(ws) }()
	time.Sleep(50 * time.Millisecond) // let the second acquire reach the flight
	close(release)
	a, b := <-first, <-second
	if opens.Load() != 1 || a == nil || a != b {
		t.Fatalf("opens = %d (first %p, second %p); want one shared open", opens.Load(), a, b)
	}

	other := t.TempDir()
	entered2, release2 := make(chan struct{}), make(chan struct{})
	p.openStore = func(ws string, _ bool) (*collab.Store, error) {
		close(entered2)
		<-release2
		return collab.Open(ws)
	}
	late := make(chan *collab.Store, 1)
	go func() { late <- p.acquire(other) }()
	<-entered2
	p.closeAll()
	close(release2)
	if s := <-late; s != nil {
		t.Fatal("a store opened after closeAll was handed out")
	}
	p.mu.Lock()
	cached := p.cachedLocked(other)
	p.mu.Unlock()
	if cached != nil {
		t.Fatal("a store opened after closeAll was published")
	}
}

// TestPool_AttachWatcherSkipsAnEntryTornDown: a watcher built for an entry that
// was torn down meanwhile (removed from the map, state untouched) is stopped,
// not attached.
func TestPool_AttachWatcherSkipsAnEntryTornDown(t *testing.T) {
	pool, ctx := fastWarmingPool(t, time.Minute)
	defer pool.close()
	pool.watcherFactory = func(string, *clientProxy) (*lspFSWatcher, error) { return nil, errSeam }
	root := goModRoot(t)
	e, err := pool.acquireLang(ctx, root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := newLSPFSWatcher(root, e.proxy)
	if err != nil {
		t.Skipf("no file watcher on this platform: %v", err)
	}
	pool.mu.Lock()
	delete(pool.entries, poolKey{root, "go"})
	pool.mu.Unlock()
	pool.watcherFactory = func(string, *clientProxy) (*lspFSWatcher, error) { return w, nil }
	pool.attachWatcher(e)
	pool.mu.Lock()
	attached := e.watcher
	pool.mu.Unlock()
	if attached != nil || !watcherStopped(w) {
		t.Fatalf("a torn-down entry got a watcher (attached %v, stopped %v)", attached != nil, watcherStopped(w))
	}
}
