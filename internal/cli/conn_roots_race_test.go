package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// Issue #514: a close() that lands between an attach's decision and its
// mutation must not leave a reference behind, and overlapping roots
// notifications must settle on the newest roots/list answer.

// poolRefsLang is poolRefs for any language: the pinned refcount on the
// (root, language) entry, or -1 when there is none. Taken under the pool lock.
func poolRefsLang(p *workspacePool, root, language string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[poolKey{root, language}]; ok {
		return e.refs
	}
	return -1
}

// goRefSession builds a connection over a pool with a pre-started go server
// for each root, so an attach pins a real pool reference without spawning
// anything. close is idempotent and registered for cleanup.
func goRefSession(t *testing.T, roots ...string) (s *connSession, pool *workspacePool, closeOnce func()) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	pool = enableTestPool()
	// Keep released entries in the map, so a released reference reads 0
	// rather than disappearing (a zero grace reaps it at once).
	pool.idleGrace = time.Hour
	for _, r := range roots {
		mustWrite(t, filepath.Join(r, "go.mod"), "module x\n")
		installEntryLang(pool, r, "go", &stubClient{id: r})
	}
	s = newConnSession(context.Background(), pool, nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	closeOnce = sync.OnceFunc(s.close)
	t.Cleanup(closeOnce)
	return s, pool, closeOnce
}

// closeAtLane arms the mutateLive probe to close the connection the first time
// an attach reaches the mutation lane, and reports whether it fired.
func closeAtLane(s *connSession, closeOnce func()) *atomic.Bool {
	fired := &atomic.Bool{}
	s.beforeLiveMutate = func() {
		if fired.CompareAndSwap(false, true) {
			closeOnce()
		}
	}
	return fired
}

// TestRootsAttach_CloseAtTheLaneTakesNoReference: close() runs after the roots
// handler's own closed-check and after Detect, immediately before the attach
// takes the mutation lane. The attach must then abort without pinning the
// language server; before #514 it committed after close() had released, and
// the reference was never released.
func TestRootsAttach_CloseAtTheLaneTakesNoReference(t *testing.T) {
	t.Run("control: the same attach without a close pins a reference", func(t *testing.T) {
		root := freshTempDir(t)
		s, pool, closeOnce := goRefSession(t, root)
		s.markInitSettled()
		s.handleRootsListChanged(context.Background(), rootsAnswer(t, root))
		if got := s.workspace(); got != root {
			t.Fatalf("workspace = %q, want %q", got, root)
		}
		if got := poolRefsLang(pool, root, "go"); got != 1 {
			t.Fatalf("refs after attach = %d, want 1", got)
		}
		closeOnce()
		if got := poolRefsLang(pool, root, "go"); got != 0 {
			t.Fatalf("refs after close = %d, want 0", got)
		}
	})
	t.Run("close at the lane", func(t *testing.T) {
		root := freshTempDir(t)
		s, pool, closeOnce := goRefSession(t, root)
		s.markInitSettled()
		fired := closeAtLane(s, closeOnce)
		s.handleRootsListChanged(context.Background(), rootsAnswer(t, root))
		if !fired.Load() {
			t.Fatal("the probe never fired: the attach did not reach mutateLive")
		}
		if got := poolRefsLang(pool, root, "go"); got != 0 {
			t.Fatalf("refs after close-then-attach = %d, want 0: the attach pinned a server close() will never release", got)
		}
		if got := s.workspace(); got != "" {
			t.Fatalf("a closed connection attached %q", got)
		}
		if s.view().qualityRunner != nil {
			t.Fatal("a closed connection started a quality runner")
		}
	})
}

// TestRootsRepin_CloseAtTheLaneTakesNoReference: the same window on the re-pin
// path. The connection holds root A; the client switches to B and close()
// lands before the re-pin's lane. A's reference is released by close(), and B
// must never be pinned.
func TestRootsRepin_CloseAtTheLaneTakesNoReference(t *testing.T) {
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	s, pool, closeOnce := goRefSession(t, rootA, rootB)
	s.markInitSettled()
	s.handleRootsListChanged(context.Background(), rootsAnswer(t, rootA))
	if got := poolRefsLang(pool, rootA, "go"); got != 1 {
		t.Fatalf("setup: refs on A = %d, want 1", got)
	}

	fired := closeAtLane(s, closeOnce)
	s.handleRootsListChanged(context.Background(), rootsAnswer(t, rootB))
	if !fired.Load() {
		t.Fatal("the probe never fired: the re-pin did not reach mutateLive")
	}
	if got := poolRefsLang(pool, rootA, "go"); got != 0 {
		t.Fatalf("refs on A = %d, want 0 (released by close)", got)
	}
	if got := poolRefsLang(pool, rootB, "go"); got != 0 {
		t.Fatalf("refs on B = %d, want 0: the re-pin committed after close()", got)
	}
}

// TestOnInitAttach_CloseAtTheLaneTakesNoReference: OnInit's attach ladder has
// the same window. Its roots rung attaches through attachWorkspace.
func TestOnInitAttach_CloseAtTheLaneTakesNoReference(t *testing.T) {
	root := freshTempDir(t)
	s, pool, closeOnce := goRefSession(t, root)
	fired := closeAtLane(s, closeOnce)
	s.attachOnInit(context.Background(), rootsAnswer(t, root))
	if !fired.Load() {
		t.Fatal("the probe never fired: the OnInit attach did not reach mutateLive")
	}
	if got := poolRefsLang(pool, root, "go"); got != 0 {
		t.Fatalf("refs after OnInit attach = %d, want 0: it committed after close()", got)
	}
}

// TestAttachSynthetic_CloseAtTheLaneCommitsNothing: a markerless root takes no
// language server, but its attach still starts a quality runner and persists
// the pin, so it must abort the same way.
func TestAttachSynthetic_CloseAtTheLaneCommitsNothing(t *testing.T) {
	root := freshTempDir(t)
	s, _, closeOnce := goRefSession(t)
	fired := closeAtLane(s, closeOnce)
	s.attachSynthetic(context.Background(), root, sessionstate.PinSourceSessionStart, pinTriggerLive)
	if !fired.Load() {
		t.Fatal("the probe never fired: the synthetic attach did not reach mutateLive")
	}
	if got := s.workspace(); got != "" {
		t.Fatalf("a closed connection attached the synthetic root %q", got)
	}
	if s.view().qualityRunner != nil {
		t.Fatal("a closed connection started a quality runner")
	}
}

// TestRefreshPrimary_CloseAtTheLaneTakesNoReference: the enable-lsp refresh
// pins a primary for an already-attached session and shares the window.
func TestRefreshPrimary_CloseAtTheLaneTakesNoReference(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	pool := enableTestPool()
	pool.idleGrace = time.Hour // see goRefSession
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, ".plumb", "config.toml"), "")
	mustWrite(t, filepath.Join(root, "index.html"), "<html></html>\n")
	installEntryLang(pool, root, "html", &stubClient{id: "html"})
	s := newConnSession(context.Background(), pool, nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	closeOnce := sync.OnceFunc(s.close)
	t.Cleanup(closeOnce)
	s.attachWorkspace(context.Background(), "file://"+root)
	if got := s.workspace(); got != root {
		t.Fatalf("setup: workspace = %q, want %q", got, root)
	}
	if already, err := pool.enableLanguage("html"); err != nil || already {
		t.Fatalf("enableLanguage(html) = (already=%v, err=%v)", already, err)
	}

	fired := closeAtLane(s, closeOnce)
	s.onBeforeTool(context.Background(), "read_file", json.RawMessage(`{}`))
	if !fired.Load() {
		t.Fatal("the probe never fired: the refresh did not reach mutateLive")
	}
	if got := poolRefsLang(pool, root, "html"); got != 0 {
		t.Fatalf("html refs = %d, want 0: the refresh pinned a server after close()", got)
	}
}

// TestBindWriteLimiterParent_NoBudgetAfterClose: the attach paths are followed
// by applyProjectConfig, which binds the shared write budget. A bind that runs
// after close() — or whose acquire lands after close() read the key — held a
// budget reference nobody released.
func TestBindWriteLimiterParent_NoBudgetAfterClose(t *testing.T) {
	setup := func(t *testing.T) (*connSession, *sharedBudgets, func()) {
		t.Helper()
		root := freshTempDir(t)
		s, _, closeOnce := goRefSession(t, root)
		budgets := newSharedBudgets()
		s.budgets = budgets
		s.mutate(func(v *sessionView) { v.clientName, v.clientVersion = "client", "1" })
		s.attachWorkspace(context.Background(), "file://"+root)
		if s.workspace() != root {
			t.Fatalf("setup: workspace = %q, want %q", s.workspace(), root)
		}
		return s, budgets, closeOnce
	}
	t.Run("control: an open connection binds a budget", func(t *testing.T) {
		s, budgets, _ := setup(t)
		s.bindWriteLimiterParent()
		if got := budgets.len(); got != 1 {
			t.Fatalf("budget entries = %d, want 1", got)
		}
	})
	t.Run("bind after close", func(t *testing.T) {
		s, budgets, closeOnce := setup(t)
		closeOnce()
		s.bindWriteLimiterParent()
		if got := budgets.len(); got != 0 {
			t.Fatalf("budget entries = %d, want 0: a bind after close() leaked one", got)
		}
	})
	t.Run("close at the lane", func(t *testing.T) {
		s, budgets, closeOnce := setup(t)
		fired := closeAtLane(s, closeOnce)
		s.bindWriteLimiterParent()
		if !fired.Load() {
			t.Fatal("the probe never fired: the bind did not reach mutateLive")
		}
		if got := budgets.len(); got != 0 {
			t.Fatalf("budget entries = %d, want 0: the bind committed after close()", got)
		}
	})
}

// TestBindWriteLimiterParent_AcquiresUnderTheLane: the budget acquire must run
// inside the mutation lane, together with publishing its key. Outside it,
// close() can read and release the key between the publish and the acquire,
// and the acquire then holds a reference nobody releases. A probe in acquire
// tries the lane: if it can take it, the acquire is not under it.
func TestBindWriteLimiterParent_AcquiresUnderTheLane(t *testing.T) {
	root := freshTempDir(t)
	s, _, _ := goRefSession(t, root)
	budgets := newSharedBudgets()
	s.budgets = budgets
	s.mutate(func(v *sessionView) { v.clientName, v.clientVersion = "client", "1" })
	s.attachWorkspace(context.Background(), "file://"+root)
	var probed, outside bool
	budgets.onAcquire = func() {
		probed = true
		if s.muMutate.TryLock() {
			s.muMutate.Unlock()
			outside = true
		}
	}
	s.bindWriteLimiterParent()
	if !probed {
		t.Fatal("the bind never acquired a budget")
	}
	if outside {
		t.Fatal("the budget was acquired outside the mutation lane: close() can release its key first")
	}
}

// scriptedRoots is a roots/list RequestFn whose FIRST call blocks until
// release is closed and then answers stale; every later call answers fresh at
// once. firstIn closes when the first call is in flight.
type scriptedRoots struct {
	calls   atomic.Int32
	firstIn chan struct{}
	release chan struct{}
	stale   mcp.RequestFn
	fresh   mcp.RequestFn
}

func newScriptedRoots(t *testing.T, stale, fresh string) *scriptedRoots {
	t.Helper()
	return &scriptedRoots{
		firstIn: make(chan struct{}),
		release: make(chan struct{}),
		stale:   rootsAnswer(t, stale),
		fresh:   rootsAnswer(t, fresh),
	}
}

func (r *scriptedRoots) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if r.calls.Add(1) == 1 {
		close(r.firstIn)
		<-r.release
		return r.stale(ctx, method, params)
	}
	return r.fresh(ctx, method, params)
}

// TestHandleRootsListChanged_FirstAnswerArrivesLast: two notifications
// overlap, and the answer to the first arrives after the second notification.
// The pin must settle on the newest answer. Before #514 the second handler
// fetched and applied B, then the first handler's late A replaced it.
func TestHandleRootsListChanged_FirstAnswerArrivesLast(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()
	s.markInitSettled()
	rr := newScriptedRoots(t, rootA, rootB)

	first := make(chan struct{})
	go func() {
		defer close(first)
		s.handleRootsListChanged(context.Background(), rr.request)
	}()
	<-rr.firstIn
	s.handleRootsListChanged(context.Background(), rr.request)
	close(rr.release)
	<-first

	if got := s.workspace(); got != rootB {
		t.Fatalf("workspace = %q, want the newest answer %q", got, rootB)
	}
	if got := rr.calls.Load(); got != 2 {
		t.Fatalf("roots/list calls = %d, want 2 (the in-flight fetch, then one re-fetch)", got)
	}
}

// TestHandleRootsListChanged_BurstDuringAFetchCostsOneMore: a burst of
// notifications that all arrive while one fetch is in flight folds into
// exactly one re-fetch, not one fetch each.
func TestHandleRootsListChanged_BurstDuringAFetchCostsOneMore(t *testing.T) {
	const burst = 8
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()
	s.markInitSettled()
	rr := newScriptedRoots(t, rootA, rootB)

	first := make(chan struct{})
	go func() {
		defer close(first)
		s.handleRootsListChanged(context.Background(), rr.request)
	}()
	<-rr.firstIn
	var wg sync.WaitGroup
	for range burst - 1 {
		wg.Go(func() { s.handleRootsListChanged(context.Background(), rr.request) })
	}
	wg.Wait()
	close(rr.release)
	<-first

	if got := rr.calls.Load(); got != 2 {
		t.Fatalf("%d notifications made %d roots/list calls, want 2", burst, got)
	}
	if got := s.workspace(); got != rootB {
		t.Fatalf("workspace = %q, want the newest answer %q", got, rootB)
	}
}

// TestHandleRootsListChanged_PanickingRunReleasesTheLoop: the mcp server
// recovers a panicking handler, so a run that panics must hand the loop back.
// Otherwise every later notification on the connection would find the loop
// owned and return without fetching.
func TestHandleRootsListChanged_PanickingRunReleasesTheLoop(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()
	s.markInitSettled()

	panicking := func(context.Context, string, any) (json.RawMessage, error) {
		panic("client transport blew up")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not propagate to the caller (safeRun)")
			}
		}()
		s.handleRootsListChanged(context.Background(), panicking)
	}()

	s.handleRootsListChanged(context.Background(), rootsAnswer(t, root))
	if got := s.workspace(); got != root {
		t.Fatalf("workspace = %q, want %q: the notification after a panicking run was dropped", got, root)
	}
}

// TestRootsCoalescer_Protocol pins the hand-off rules the handler relies on.
func TestRootsCoalescer_Protocol(t *testing.T) {
	answer := func(tag string) mcp.RequestFn {
		return func(context.Context, string, any) (json.RawMessage, error) {
			return json.RawMessage(tag), nil
		}
	}
	tagOf := func(t *testing.T, fn mcp.RequestFn) string {
		t.Helper()
		raw, _ := fn(context.Background(), "", nil)
		return string(raw)
	}
	live := func() bool { return true }

	var c rootsCoalescer
	if !c.enter(answer("1")) {
		t.Fatal("first notification did not take the loop")
	}
	if fn, ok := c.next(live); !ok || tagOf(t, fn) != "1" {
		t.Fatal("the owner did not get the first request")
	}
	// Two notifications during the run fold into one re-run with the newest.
	if c.enter(answer("2")) || c.enter(answer("3")) {
		t.Fatal("a notification during a run took the loop: two runs in flight")
	}
	if fn, ok := c.next(live); !ok || tagOf(t, fn) != "3" {
		t.Fatal("the re-run did not use the newest request")
	}
	if _, ok := c.next(live); ok {
		t.Fatal("a third run with nothing outstanding")
	}
	// The owner has released the loop, so the next notification must take it,
	// not be parked behind a loop that has already exited.
	if !c.enter(answer("4")) {
		t.Fatal("a notification after the loop exited was lost")
	}
	// A closed connection ends the loop and releases it.
	if _, ok := c.next(func() bool { return false }); ok {
		t.Fatal("next ran on a closed connection")
	}
	if !c.enter(answer("5")) {
		t.Fatal("the loop was not released when the connection closed")
	}
}

// watchSession builds a bare connection on a real project-config watch
// manager, plus a closer that runs close()'s own order for the watcher:
// cancel, then releaseProjectWatch. A directory the manager will watch must
// come from watchedTempDir(t, m), never t.TempDir: this helper registers the
// manager's clean-up first, so a plain t.TempDir created after it is removed
// BEFORE the manager closes, which is the delete-during-close race that
// double-closes a descriptor in fsnotify's kqueue backend (see closeFSWatcher).
func watchSession(t *testing.T) (s *connSession, m *projectConfigWatchManager, closeLike func()) {
	t.Helper()
	m, _ = testWatchManager(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	s = &connSession{ctx: ctx, cancel: cancel, store: config.NewStore(config.Defaults()), projectWatches: m}
	closeLike = sync.OnceFunc(func() { s.cancel(); s.releaseProjectWatch() })
	t.Cleanup(closeLike)
	return s, m, closeLike
}

// TestTrackProjectWatch_NoWatchOutlivesClose: the project-config watcher
// reference follows the same rule as the language server (issue #514). Before
// the fix trackProjectWatch published the root and acquired afterwards, so a
// close() in between released first and the acquire leaked.
func TestTrackProjectWatch_NoWatchOutlivesClose(t *testing.T) {
	t.Run("control: one reference, released on close", func(t *testing.T) {
		s, m, closeLike := watchSession(t)
		ws := watchedTempDir(t, m)
		s.trackProjectWatch(ws)
		if got := m.refs(ws); got != 1 {
			t.Fatalf("refs = %d, want 1", got)
		}
		s.trackProjectWatch(ws) // same root: no extra reference
		if got := m.refs(ws); got != 1 {
			t.Fatalf("refs after a repeat = %d, want 1", got)
		}
		closeLike()
		if got := m.refs(ws); got != 0 {
			t.Fatalf("refs after close = %d, want 0", got)
		}
	})
	t.Run("close at the lane", func(t *testing.T) {
		s, m, closeLike := watchSession(t)
		ws := watchedTempDir(t, m)
		fired := closeAtLane(s, closeLike)
		s.trackProjectWatch(ws)
		if !fired.Load() {
			t.Fatal("the probe never fired: trackProjectWatch did not reach mutateLive")
		}
		if got := m.refs(ws); got != 0 {
			t.Fatalf("refs = %d, want 0: a watcher reference outlived close()", got)
		}
	})
	t.Run("the reference is held before it is published", func(t *testing.T) {
		s, m, _ := watchSession(t)
		ws := watchedTempDir(t, m)
		refsAtLane := -1
		s.beforeLiveMutate = func() { refsAtLane = m.refs(ws) }
		s.trackProjectWatch(ws)
		if refsAtLane != 1 {
			t.Fatalf("refs when publishing = %d, want 1: a close() between the publish and the acquire would release first and the acquire would leak", refsAtLane)
		}
	})
	t.Run("a concurrent track of the same root keeps one reference", func(t *testing.T) {
		s, m, _ := watchSession(t)
		ws := watchedTempDir(t, m)
		// The first track, holding its reference but not yet published, lets a
		// second track of the same root run to completion. The first then finds
		// the root already published and must drop its own reference.
		var nested atomic.Bool
		s.beforeLiveMutate = func() {
			if nested.CompareAndSwap(false, true) {
				s.trackProjectWatch(ws)
			}
		}
		s.trackProjectWatch(ws)
		if got := m.refs(ws); got != 1 {
			t.Fatalf("refs = %d, want 1: two tracks of one root left two references", got)
		}
	})
	t.Run("a re-pin releases the previous root", func(t *testing.T) {
		s, m, _ := watchSession(t)
		ws, ws2 := watchedTempDir(t, m), watchedTempDir(t, m)
		s.trackProjectWatch(ws)
		s.trackProjectWatch(ws2)
		if a, b := m.refs(ws), m.refs(ws2); a != 0 || b != 1 {
			t.Fatalf("refs = (%d, %d), want (0, 1)", a, b)
		}
	})
}

// TestTrackProjectWatch_CloseRaceStress races close()'s watcher teardown
// against trackProjectWatch with no seam at all. A leaked reference shows up
// as a nonzero count once both have finished.
func TestTrackProjectWatch_CloseRaceStress(t *testing.T) {
	const iterations = 3000
	m, _ := testWatchManager(t, nil)
	ws := watchedTempDir(t, m)
	leaks := 0
	for range iterations {
		ctx, cancel := context.WithCancel(context.Background())
		s := &connSession{ctx: ctx, cancel: cancel, store: config.NewStore(config.Defaults()), projectWatches: m}
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Go(func() { <-start; s.trackProjectWatch(ws) })
		wg.Go(func() { <-start; s.cancel(); s.releaseProjectWatch() })
		close(start)
		wg.Wait()
		if m.refs(ws) != 0 {
			leaks++
			for m.refs(ws) > 0 {
				m.release(ws)
			}
		}
	}
	if leaks > 0 {
		t.Fatalf("%d/%d iterations leaked a watcher reference past close()", leaks, iterations)
	}
}

// TestBindWriteLimiterParent_ReparentsBeforeReleasingTheOldBudget: a re-bind
// must point the write limiter at the new budget before the old one is
// released. Re-parenting after the release left a window in which the limiter
// charged a budget that might already be gone, and let two concurrent binds
// finish with the limiter on the key that was NOT bound. The probe in release
// charges one write and checks where it landed.
func TestBindWriteLimiterParent_ReparentsBeforeReleasingTheOldBudget(t *testing.T) {
	root := freshTempDir(t)
	s, _, _ := goRefSession(t, root)
	budgets := newSharedBudgets()
	s.budgets = budgets
	// A cap no test reaches, so Allow always records against the parent.
	s.writeLimiter = tools.NewRateLimiter(1<<30, time.Minute)
	s.mutate(func(v *sessionView) { v.clientName, v.clientVersion = "client", "a" })
	s.attachWorkspace(context.Background(), "file://"+root)
	s.bindWriteLimiterParent()
	oldKey := s.view().boundBudgetKey
	if oldKey == "" {
		t.Fatal("setup: no budget bound")
	}
	charged := func(key string) int {
		budgets.mu.Lock()
		e, ok := budgets.m[key]
		budgets.mu.Unlock()
		if !ok {
			return -1
		}
		n, _, _ := e.limiter.Snapshot()
		return n
	}

	s.mutate(func(v *sessionView) { v.clientVersion = "b" })
	var probed, onNew bool
	budgets.onRelease = func(key string) {
		if key != oldKey {
			return
		}
		probed = true
		newKey := s.view().boundBudgetKey
		before := charged(newKey)
		if !s.writeLimiter.Allow() {
			t.Error("the limiter refused under a cap no test reaches")
		}
		onNew = charged(newKey) == before+1
	}
	s.bindWriteLimiterParent()
	if !probed {
		t.Fatal("the re-bind never released the old budget")
	}
	if !onNew {
		t.Fatal("the old budget was released while the limiter still pointed at it: re-parent before releasing")
	}
}

// TestBindWriteLimiterParent_ConcurrentBindsKeepTheBoundParent: two binds
// racing to different keys must leave the limiter parented to the budget of
// the key that ended up bound. Seam-free: re-parenting anywhere outside the
// lane — even before the old budget's release, which the release probe above
// accepts — lets the bind that published first re-parent last. Such a round
// charges a budget other than the bound key's. Measured on that mutant: about
// 10 bad rounds per 20000, so this round count fails it with near certainty.
func TestBindWriteLimiterParent_ConcurrentBindsKeepTheBoundParent(t *testing.T) {
	const rounds = 20000
	root := freshTempDir(t)
	s, _, _ := goRefSession(t, root)
	budgets := newSharedBudgets()
	s.budgets = budgets
	// A cap no test reaches, so Allow always records against the parent.
	s.writeLimiter = tools.NewRateLimiter(1<<30, time.Minute)
	s.mutate(func(v *sessionView) { v.clientName, v.clientVersion = "client", "a" })
	s.attachWorkspace(context.Background(), "file://"+root)
	charged := func(key string) int {
		budgets.mu.Lock()
		e, ok := budgets.m[key]
		budgets.mu.Unlock()
		if !ok {
			return -1
		}
		n, _, _ := e.limiter.Snapshot()
		return n
	}
	bad := 0
	for range rounds {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, ver := range []string{"a", "b"} {
			wg.Go(func() {
				<-start
				s.mutate(func(v *sessionView) { v.clientVersion = ver })
				s.bindWriteLimiterParent()
			})
		}
		close(start)
		wg.Wait()
		key := s.view().boundBudgetKey
		before := charged(key)
		if !s.writeLimiter.Allow() {
			t.Fatal("the limiter refused under a cap no test reaches")
		}
		if charged(key) != before+1 {
			bad++
		}
	}
	if bad > 0 {
		t.Fatalf("%d/%d rounds left the limiter charging a budget other than the bound key's", bad, rounds)
	}
}

// laneProbeCtx is a context whose Err records, while armed, whether it was
// consulted and whether any call ran with the mutation lane free.
type laneProbeCtx struct {
	context.Context
	s       *connSession
	armed   *atomic.Bool
	outside *atomic.Bool
	calls   *atomic.Int32
}

func (c laneProbeCtx) Err() error {
	if c.armed.Load() {
		c.calls.Add(1)
		if c.s.muMutate.TryLock() {
			c.s.muMutate.Unlock()
			c.outside.Store(true)
		}
	}
	return c.Context.Err()
}

// TestMutateLive_ChecksTheConnectionInsideTheLane: the close-at-the-lane tests
// call close() from beforeLiveMutate, so a check placed after that seam but
// before the lock would pass them while leaving the window open. This pins the
// check itself: every consultation of the connection context must find the
// lane held.
func TestMutateLive_ChecksTheConnectionInsideTheLane(t *testing.T) {
	s := &connSession{}
	armed, outside, calls := &atomic.Bool{}, &atomic.Bool{}, &atomic.Int32{}
	s.ctx = laneProbeCtx{Context: context.Background(), s: s, armed: armed, outside: outside, calls: calls}
	armed.Store(true)
	if !s.mutateLive(func(*sessionView) {}) {
		t.Fatal("mutateLive reported closed on an open connection")
	}
	armed.Store(false)
	if calls.Load() == 0 {
		t.Fatal("mutateLive never consulted the connection context")
	}
	if outside.Load() {
		t.Fatal("mutateLive checked the connection outside the lane: a close() can land between the check and the lock")
	}
}
