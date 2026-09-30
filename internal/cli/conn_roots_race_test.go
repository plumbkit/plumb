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

// TestHandleRootsListChanged_BurstCostsAtMostTwoFetches: a burst of
// notifications that all arrive while one fetch is in flight folds into
// exactly one re-fetch.
func TestHandleRootsListChanged_BurstCostsAtMostTwoFetches(t *testing.T) {
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
