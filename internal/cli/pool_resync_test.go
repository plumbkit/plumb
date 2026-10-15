package cli

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
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

// TestWatcherFailed_RetriesUntilAWatcherAttaches: a recreation whose new
// watcher cannot be created (the limit that failed the first) keeps trying.
func TestWatcherFailed_RetriesUntilAWatcherAttaches(t *testing.T) {
	pool, f := resyncPool(t)
	pool.resyncQuiet = time.Hour
	root := goModRoot(t)
	e, err := pool.acquireLang(t.Context(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	pool.watcherFactory = func(r string, proxy *clientProxy) (*lspFSWatcher, error) {
		if calls.Add(1) <= 2 {
			return nil, errors.New("seam: inotify watch limit")
		}
		return f.factory(r, proxy)
	}
	close(f.latest(root).failed)
	eventually(t, 5*time.Second, "the recreation gave up after a failed attach", func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return e.watcher != nil && calls.Load() >= 3
	})
	pool.mu.Lock()
	healthy := e.watcherHealthySince
	pool.mu.Unlock()
	if healthy.IsZero() {
		t.Fatal("a recreated watcher did not record when it became healthy")
	}
}

// TestWatcherFailed_AStaleRecreationLeavesAHealthyWatcher: when another path
// (a wake, a restart) attached a healthy watcher during the backoff, the
// recreation for the old, failed one does nothing.
func TestWatcherFailed_AStaleRecreationLeavesAHealthyWatcher(t *testing.T) {
	pool, f := resyncPool(t)
	pool.resyncQuiet = time.Hour
	pool.watcherRetryDelay = 200 * time.Millisecond
	root := goModRoot(t)
	e, err := pool.acquireLang(t.Context(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	close(f.latest(root).failed)
	pool.mu.Lock()
	old := e.watcher
	e.watcher = nil
	pool.mu.Unlock()
	old.Stop()
	if !pool.attachWatcher(e) { // the healthy replacement another path made
		t.Fatal("control: could not attach the replacement")
	}
	pool.mu.Lock()
	healthy := e.watcher
	pool.mu.Unlock()
	time.Sleep(500 * time.Millisecond)
	pool.mu.Lock()
	now := e.watcher
	pool.mu.Unlock()
	if now != healthy || f.count(root) != 2 {
		t.Fatalf("the stale recreation replaced the healthy watcher (%d watchers built)", f.count(root))
	}
}

// TestWatcherLost_ARecurringStormWidensTheInterval: resyncs that follow each
// other closely count a streak, which doubles the minimum interval.
func TestWatcherLost_ARecurringStormWidensTheInterval(t *testing.T) {
	pool, _ := resyncPool(t)
	pool.resyncMinInterval = 20 * time.Millisecond
	root := goModRoot(t)
	e, err := pool.acquireLang(t.Context(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		pool.mu.Lock()
		e.reconfiguring = true // count the resync without a restart
		pool.mu.Unlock()
		pool.resyncEntry(e)
		time.Sleep(5 * time.Millisecond)
	}
	pool.mu.Lock()
	streak := e.resyncStreak
	e.reconfiguring = false
	pool.mu.Unlock()
	if streak < 2 {
		t.Fatalf("resync streak = %d after three back-to-back resyncs, want it to grow", streak)
	}
}

// TestWatcherLost_APendingResyncIsDroppedWhenTheServerHibernates.
func TestWatcherLost_APendingResyncIsDroppedWhenTheServerHibernates(t *testing.T) {
	pool, f := resyncPool(t)
	pool.resyncQuiet = time.Hour
	root := goModRoot(t)
	e, err := pool.acquireLang(t.Context(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	f.latest(root).lost <- struct{}{}
	eventually(t, 3*time.Second, "control: no resync was scheduled", func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return e.resyncTimer != nil
	})
	pool.hibernateEntry(e)
	pool.mu.Lock()
	pending := e.resyncTimer != nil
	pool.mu.Unlock()
	if pending {
		t.Fatal("a hibernated server kept its pending resync")
	}
}

// TestWatcherFailed_ALongHealthyRunResetsTheBackoff.
func TestWatcherFailed_ALongHealthyRunResetsTheBackoff(t *testing.T) {
	pool, _ := resyncPool(t)
	pool.watcherRetryDelay = time.Hour // park the recreation after its first count
	root := goModRoot(t)
	e, err := pool.acquireLang(t.Context(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	e.watcherFailures = 6
	e.watcherHealthySince = time.Now().Add(-lspWatcherRetryMax - time.Minute)
	w := e.watcher
	pool.mu.Unlock()
	pool.onWatcherFailed(e, w)
	eventually(t, 3*time.Second, "the backoff was not reset after a long healthy run", func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return e.watcherFailures == 1
	})
}

// TestWatcherFailed_AFailureDuringARecreationRunsAnotherRound: Failed fires
// once per watcher, so one that arrives while a recreation runs is queued and
// handled when it ends, not dropped.
func TestWatcherFailed_AFailureDuringARecreationRunsAnotherRound(t *testing.T) {
	pool, f := resyncPool(t)
	pool.resyncQuiet = time.Hour
	root := goModRoot(t)
	e, err := pool.acquireLang(t.Context(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	e.watcherRetrying = true // a recreation is under way
	cur := e.watcher
	pool.mu.Unlock()
	pool.onWatcherFailed(e, cur) // the current watcher fails meanwhile
	pool.mu.Lock()
	queued := e.watcherRetryAgain
	pool.mu.Unlock()
	if !queued {
		t.Fatal("a failure during a recreation was dropped")
	}
	before := f.count(root)
	// The running recreation ends (its own watcher was replaced elsewhere): the
	// queued failure must start another round, which replaces cur.
	pool.recreateWatcher(e, &lspFSWatcher{})
	eventually(t, 3*time.Second, "the queued failure did not run another round", func() bool {
		return f.count(root) > before
	})
}

func TestResyncInterval_DoublesWithTheStreakUpToAnHour(t *testing.T) {
	for streak, want := range map[int]time.Duration{0: 10 * time.Minute, 1: 20 * time.Minute, 2: 40 * time.Minute, 3: time.Hour, 9: time.Hour} {
		if got := resyncInterval(lspResyncMinInterval, streak); got != want {
			t.Errorf("resyncInterval(streak %d) = %v, want %v", streak, got, want)
		}
	}
	base := time.Now()
	if s := nextResyncStreak(base, base.Add(15*time.Minute), lspResyncMinInterval, 0); s != 1 {
		t.Errorf("a resync 15 min after the last (interval 10 min) is not part of a streak: %d", s)
	}
	if s := nextResyncStreak(base, base.Add(25*time.Minute), lspResyncMinInterval, 0); s != 0 {
		t.Errorf("a resync 25 min after the last (interval 10 min) did not reset the streak: %d", s)
	}
	if s := nextResyncStreak(time.Time{}, base, lspResyncMinInterval, 4); s != 0 {
		t.Errorf("a first resync carried a streak: %d", s)
	}
}

// TestAttachWatcher_ReportsWhetherItAttached.
func TestAttachWatcher_ReportsWhetherItAttached(t *testing.T) {
	pool, _ := resyncPool(t)
	e, err := pool.acquireLang(t.Context(), goModRoot(t), "go", true)
	if err != nil {
		t.Fatal(err)
	}
	pool.hibernateEntry(e)
	if pool.attachWatcher(e) {
		t.Fatal("attachWatcher reported success on a hibernated entry")
	}
}

// TestWatcherLost_ASkippedHibernationKeepsThePendingResync: a hibernation
// refused because a call is in progress leaves the pending resync in place.
func TestWatcherLost_ASkippedHibernationKeepsThePendingResync(t *testing.T) {
	pool, f := resyncPool(t)
	pool.resyncQuiet = time.Hour
	root := goModRoot(t)
	e, done, err := pool.acquireCall(t.Context(), root, "go")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	f.latest(root).lost <- struct{}{}
	eventually(t, 3*time.Second, "control: no resync was scheduled", func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return e.resyncTimer != nil
	})
	pool.hibernateEntry(e) // refused: a call holds it
	pool.mu.Lock()
	pending := e.resyncTimer != nil
	pool.mu.Unlock()
	if !pending {
		t.Fatal("a refused hibernation dropped the pending resync")
	}
}
