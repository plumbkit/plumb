package cli

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/fswatch"
	"github.com/plumbkit/plumb/internal/lsp"
)

// These tests pin PLAN-538: a watcher's lost events resync that entry's server
// only, a storm collapses into one resync, and a failed watcher is recreated
// and the server resynced.

// fakeWatchers is a watcherFactory whose watchers a test drives.
type fakeWatchers struct {
	mu    sync.Mutex
	byDir map[string][]*fakeWatch
}

type fakeWatch struct {
	lost   chan struct{}
	failed chan struct{}
}

func (f *fakeWatchers) factory(root string, proxy *clientProxy) (*lspFSWatcher, error) {
	fw := &fakeWatch{lost: make(chan struct{}, 1), failed: make(chan struct{})}
	f.mu.Lock()
	if f.byDir == nil {
		f.byDir = map[string][]*fakeWatch{}
	}
	f.byDir[root] = append(f.byDir[root], fw)
	f.mu.Unlock()
	return &lspFSWatcher{
		workspace: root, client: proxy,
		events: make(chan fswatch.Event), lost: fw.lost, failed: fw.failed,
		closeOS: func() {}, done: make(chan struct{}),
	}, nil
}

func (f *fakeWatchers) latest(root string) *fakeWatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	ws := f.byDir[root]
	if len(ws) == 0 {
		return nil
	}
	return ws[len(ws)-1]
}

func (f *fakeWatchers) count(root string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byDir[root])
}

func supOf(p *workspacePool, e *poolEntry) *lsp.Supervisor {
	p.mu.Lock()
	defer p.mu.Unlock()
	return e.sup
}

func eventually(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func resyncPool(t *testing.T) (*workspacePool, *fakeWatchers) {
	t.Helper()
	pool, _ := fastWarmingPool(t, time.Minute)
	t.Cleanup(pool.close)
	f := &fakeWatchers{}
	pool.watcherFactory = f.factory
	pool.resyncQuiet = 50 * time.Millisecond
	pool.resyncMinInterval = time.Hour
	pool.watcherRetryDelay = 20 * time.Millisecond
	return pool, f
}

func TestWatcherLost_ResyncsOnlyThatServer(t *testing.T) {
	pool, f := resyncPool(t)
	ctx := t.Context()
	rootA, rootB := goModRoot(t), goModRoot(t)
	a, err := pool.acquireLang(ctx, rootA, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pool.acquireLang(ctx, rootB, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	supA, supB := supOf(pool, a), supOf(pool, b)
	f.latest(rootA).lost <- struct{}{}
	eventually(t, 5*time.Second, "root A's server was not resynced after its watcher lost events", func() bool {
		return supOf(pool, a) != supA
	})
	if supOf(pool, b) != supB {
		t.Fatal("root B's server was restarted by root A's lost events")
	}
	pool.mu.Lock()
	n, last := pool.resyncs, pool.lastResynced
	pool.mu.Unlock()
	if n != 1 || last.root != rootA {
		t.Fatalf("resyncs = %d, last = %v; want 1 for root A", n, last)
	}
	if s := pool.budgetSummary(); !strings.Contains(s, "resynced after lost file events: 1") || !strings.Contains(s, rootA) {
		t.Fatalf("daemon_info's pool row does not report the resync: %q", s)
	}
	var rowA []string
	for line := range strings.SplitSeq(pool.lspStatusReportV2(), "\n") {
		if f := strings.Split(line, "\t"); len(f) > 1 && f[1] == rootA {
			rowA = f
		}
	}
	if len(rowA) != 11 || rowA[10] == "-1" {
		t.Fatalf("lsp-status-v2 row for root A = %q, want its last resync", rowA)
	}
}

// TestWatcherLost_AStormIsOneResync: repeated losses inside the quiet window
// and the minimum interval cost one restart.
func TestWatcherLost_AStormIsOneResync(t *testing.T) {
	pool, f := resyncPool(t)
	root := goModRoot(t)
	e, err := pool.acquireLang(t.Context(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		select {
		case f.latest(root).lost <- struct{}{}:
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	eventually(t, 5*time.Second, "no resync after the storm", func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return pool.resyncs == 1
	})
	for range 5 { // more loss inside the minimum interval: deferred, not now
		select {
		case f.latest(root).lost <- struct{}{}:
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	pool.mu.Lock()
	n, pending := pool.resyncs, e.resyncTimer != nil
	pool.mu.Unlock()
	if n != 1 || !pending {
		t.Fatalf("resyncs = %d (pending %v); want the storm to cost one resync and the next deferred", n, pending)
	}
}

// TestWatcherFailed_IsRecreatedAndTheServerResynced.
func TestWatcherFailed_IsRecreatedAndTheServerResynced(t *testing.T) {
	pool, f := resyncPool(t)
	root := goModRoot(t)
	e, err := pool.acquireLang(t.Context(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	sup := supOf(pool, e)
	close(f.latest(root).failed)
	eventually(t, 5*time.Second, "the failed watcher was not recreated", func() bool { return f.count(root) >= 2 })
	eventually(t, 5*time.Second, "the server was not resynced after its watcher came back", func() bool {
		return supOf(pool, e) != sup
	})
}

// TestWatcherLost_AHibernatedServerIsNotRestarted: it re-reads the tree when it
// wakes.
func TestWatcherLost_AHibernatedServerIsNotRestarted(t *testing.T) {
	pool, _ := resyncPool(t)
	root := goModRoot(t)
	e, err := pool.acquireLang(t.Context(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	pool.hibernateEntry(e)
	sup := supOf(pool, e)
	pool.resyncEntry(e)
	time.Sleep(100 * time.Millisecond)
	if supOf(pool, e) != sup {
		t.Fatal("a hibernated server was restarted for lost events")
	}
}

// TestWatcherFailed_IsReattachedBeforeAnyResync: the recreated watcher is
// attached by the recreation itself (the resync is held off here, so its restart
// cannot be what builds one).
func TestWatcherFailed_IsReattachedBeforeAnyResync(t *testing.T) {
	pool, f := resyncPool(t)
	pool.resyncQuiet = time.Hour
	root := goModRoot(t)
	e, err := pool.acquireLang(t.Context(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	first := e.watcher
	pool.mu.Unlock()
	close(f.latest(root).failed)
	eventually(t, 5*time.Second, "the failed watcher was not replaced on the entry", func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return e.watcher != nil && e.watcher != first
	})
	pool.mu.Lock()
	n := pool.resyncs
	pool.mu.Unlock()
	if n != 0 {
		t.Fatal("control: a resync ran although it was held off")
	}
}
