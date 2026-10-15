package cli

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/topology"
)

// These tests pin PLAN-536: slow work — a file watcher's initial walk, opening a
// topology, memory or collab store — runs WITHOUT the owning pool's mutex, so one
// slow root never stalls another root's acquire. Each slow seam probes the mutex
// with TryLock: a seam run under the lock would find it held.

// lockProbe records whether a mutex was free when probed.
type lockProbe struct{ heldOnce atomic.Bool }

func (lp *lockProbe) probe(mu *sync.Mutex) {
	if mu.TryLock() {
		mu.Unlock()
		return
	}
	lp.heldOnce.Store(true)
}

func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s: blocked behind another root's slow work", what, d)
	}
}

var errSeam = errors.New("seam: not opened")

// TestPool_SlowWatcherDoesNotBlockAnotherRoot: a start whose file watcher is
// still walking root A neither holds the pool mutex nor stalls an acquire for
// root B; the wake path builds its watcher unlocked too.
func TestPool_SlowWatcherDoesNotBlockAnotherRoot(t *testing.T) {
	pool, ctx := fastWarmingPool(t, time.Minute)
	defer pool.close()
	rootA, rootB := goModRoot(t), goModRoot(t)
	var probe lockProbe
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	pool.watcherFactory = func(root string, _ *clientProxy) (*lspFSWatcher, error) {
		probe.probe(&pool.mu)
		if root == rootA {
			entered <- struct{}{}
			<-release
		}
		return nil, errSeam // "watcher unavailable": the entry runs without one
	}

	go func() { _, _ = pool.acquireLang(ctx, rootA, "go", true) }()
	<-entered
	within(t, 2*time.Second, "acquire of root B", func() {
		if _, err := pool.acquireLang(ctx, rootB, "go", true); err != nil {
			t.Errorf("acquire B: %v", err)
		}
	})
	close(release)
	if probe.heldOnce.Load() {
		t.Fatal("the watcher was built under the pool mutex")
	}

	// The wake path: hibernate B and acquire it again.
	e := pool.lookup(rootB, "go")
	pool.hibernateEntry(e)
	if _, err := pool.acquireLang(ctx, rootB, "go", true); err != nil {
		t.Fatalf("wake B: %v", err)
	}
	if probe.heldOnce.Load() {
		t.Fatal("the watcher was rebuilt under the pool mutex on wake")
	}
}

// TestPool_AttachWatcherSkipsAnEntryThatLeft: a watcher finished after its entry
// was hibernated is stopped, not attached to a stopped server.
func TestPool_AttachWatcherSkipsAnEntryThatLeft(t *testing.T) {
	pool, ctx := fastWarmingPool(t, time.Minute)
	defer pool.close()
	root := goModRoot(t)
	var built atomic.Int32
	pool.watcherFactory = func(string, *clientProxy) (*lspFSWatcher, error) {
		built.Add(1)
		return nil, errSeam
	}
	e, err := pool.acquireLang(ctx, root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	pool.hibernateEntry(e)
	w, err := newLSPFSWatcher(root, e.proxy)
	if err != nil {
		t.Skipf("no file watcher on this platform: %v", err)
	}
	pool.watcherFactory = func(string, *clientProxy) (*lspFSWatcher, error) { return w, nil }
	pool.attachWatcher(e)
	pool.mu.Lock()
	attached := e.watcher
	pool.mu.Unlock()
	if attached != nil {
		t.Fatal("a watcher was attached to a hibernated entry")
	}
}

// TestTopologyPool_OpenIsUnlockedAndCoalesced: opening one root's store does
// not hold the pool mutex or stall another root, and two acquires of one root
// share a single open.
func TestTopologyPool_OpenIsUnlockedAndCoalesced(t *testing.T) {
	p := newTopologyPool(config.TopologyConfig{Enabled: true})
	defer p.StopAll()
	rootA, rootB := t.TempDir(), t.TempDir()
	var probe lockProbe
	var opensA atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{})
	p.openStore = func(root string, cfg config.TopologyConfig) (*topology.Store, error) {
		probe.probe(&p.mu)
		if root != rootA {
			return nil, errSeam
		}
		if opensA.Add(1) == 1 {
			close(entered)
			<-release
		}
		return topology.Open(root, cfg, nil)
	}
	cfg := config.TopologyConfig{Enabled: true}
	var first, second *topology.Store
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); first = p.Acquire(rootA, cfg) }()
	<-entered
	wg.Add(1)
	go func() { defer wg.Done(); second = p.Acquire(rootA, cfg) }()
	within(t, 2*time.Second, "Acquire of root B", func() { p.Acquire(rootB, cfg) })
	close(release)
	wg.Wait()
	if probe.heldOnce.Load() {
		t.Fatal("a topology store was opened under the pool mutex")
	}
	if n := opensA.Load(); n != 1 || first == nil || first != second {
		t.Fatalf("root A opened %d times (first %p, second %p); want one shared open", n, first, second)
	}
}

// TestTopologyPool_ReconcileReopensUnlocked: a global retune re-opens every
// store without the pool mutex.
func TestTopologyPool_ReconcileReopensUnlocked(t *testing.T) {
	p := newTopologyPool(config.TopologyConfig{Enabled: true})
	defer p.StopAll()
	root := t.TempDir()
	var probe lockProbe
	var opens atomic.Int32
	p.openStore = func(root string, cfg config.TopologyConfig) (*topology.Store, error) {
		probe.probe(&p.mu)
		opens.Add(1)
		return topology.Open(root, cfg, nil)
	}
	if p.Acquire(root, config.TopologyConfig{Enabled: true}) == nil {
		t.Fatal("control: the first open failed")
	}
	p.Reconcile(config.TopologyConfig{Enabled: true, ResyncIntervalMinutes: 7})
	if opens.Load() != 2 {
		t.Fatalf("opens = %d, want 2 (the reconcile re-open)", opens.Load())
	}
	if probe.heldOnce.Load() {
		t.Fatal("Reconcile re-opened a store under the pool mutex")
	}
}

// TestMemoryPool_OpenIsUnlockedAndClosedAfterShutdown: opening an index does
// not hold the pool mutex or stall another workspace; an open that finishes
// after CloseAll is closed, not published.
func TestMemoryPool_OpenIsUnlockedAndClosedAfterShutdown(t *testing.T) {
	p := newMemoryIndexPool()
	wsA, wsB := t.TempDir(), t.TempDir()
	var probe lockProbe
	release := make(chan struct{})
	entered := make(chan struct{})
	p.openIndex = func(ws string) (*memory.Index, error) {
		probe.probe(&p.mu)
		if ws != wsA {
			return nil, errSeam
		}
		close(entered)
		<-release
		return memory.OpenIndex(ws)
	}
	result := make(chan *memory.Index, 1)
	go func() { result <- p.Acquire(wsA) }()
	<-entered
	within(t, 2*time.Second, "Acquire of workspace B", func() { p.Acquire(wsB) })
	p.CloseAll()
	close(release)
	if ix := <-result; ix != nil {
		t.Fatal("an index opened after CloseAll was handed out")
	}
	p.mu.Lock()
	n := len(p.indexes)
	p.mu.Unlock()
	if n != 0 {
		t.Fatal("an index opened after CloseAll was published")
	}
	if probe.heldOnce.Load() {
		t.Fatal("an index was opened under the pool mutex")
	}
}

// TestCollabPool_OpenIsUnlocked: opening a collab store (its schema migration)
// does not hold the pool mutex or stall another store, the global one included.
func TestCollabPool_OpenIsUnlocked(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	p := newCollabPool()
	defer p.closeAll()
	wsA, wsB := t.TempDir(), t.TempDir()
	var probe lockProbe
	release := make(chan struct{})
	entered := make(chan struct{})
	p.openStore = func(ws string, global bool) (*collab.Store, error) {
		probe.probe(&p.mu)
		if ws == wsA {
			close(entered)
			<-release
		}
		if global {
			return collab.OpenGlobal()
		}
		return collab.Open(ws)
	}
	got := make(chan *collab.Store, 1)
	go func() { got <- p.acquire(wsA) }()
	<-entered
	within(t, 2*time.Second, "acquire of workspace B", func() {
		if p.acquire(wsB) == nil {
			t.Error("acquire B failed")
		}
	})
	within(t, 2*time.Second, "acquire of the global store", func() { p.acquireGlobal() })
	close(release)
	if <-got == nil {
		t.Fatal("acquire A failed")
	}
	if probe.heldOnce.Load() {
		t.Fatal("a collab store was opened under the pool mutex")
	}
	_ = context.Background()
}
