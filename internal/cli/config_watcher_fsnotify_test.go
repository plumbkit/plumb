package cli

// config_watcher_fsnotify_test.go — the hardening around fsnotify's kqueue
// descriptor race (config_watcher_fsnotify.go): a watcher that lost its own
// descriptor is recreated instead of left dead, and closing waits until the
// watcher's reader has stopped delivering.
//
// A real lost descriptor cannot be produced on demand — it needs another
// goroutine's stray close to land in a microsecond window — so these tests
// inject the exact error fsnotify's reader sends once that has happened, into
// the loop through its test seam, while a real watcher the code under test
// built stays the one it must close and replace.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
)

func TestFSWatcherLost(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"kqueue reader on a closed queue", lostWatcherErr(), true},
		{"attach wrapped by fsnotify", fmt.Errorf("%q: %w", "/ws/.plumb", syscall.EBADF), true},
		{"nil", nil, false},
		{"overflow notice", fsnotify.ErrEventOverflow, false},
		{"unrelated errno", fmt.Errorf("fsnotify.dirChange: %w", syscall.EACCES), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fsWatcherLost(tt.err); got != tt.want {
				t.Errorf("fsWatcherLost(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestProjectWatchManager_RecreatesLostWatcher is the CI flake's victim side:
// a watcher whose kqueue was closed underneath it used to sit dead (its reader
// spinning on EBADF) with the workspace left to the 30s poll, so a config
// change went unseen for the whole 10s wait. It must instead be replaced at
// once, the dead one closed, and a reload scheduled for anything missed.
func TestProjectWatchManager_RecreatesLostWatcher(t *testing.T) {
	m, sig := testWatchManager(t, nil)
	built := captureWatchers(m)
	ws := watchedTempDir(t, m)
	root := paths.Canonical(ws)
	writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	m.acquire(ws)
	first, firstPlumb := nextLoopWatchers(t, built)
	// An earlier ordinary error set the failed latch (poll engaged); a
	// successful recreate is a fresh watcher, so it must clear the latch.
	m.testErrs <- fsnotify.ErrEventOverflow
	// The loop takes the next error only once it has handled this one, so a
	// second send is the barrier that makes the latch observable.
	m.testErrs <- fsnotify.ErrEventOverflow
	if m.healthy(ws) {
		t.Fatal("healthy = true after a watcher error; the precondition for the latch check below is missing")
	}

	m.testErrs <- lostWatcherErr()
	second, secondPlumb := nextLoopWatchers(t, built)
	// The replacements are opened only after the dead watchers' readers have
	// stopped delivering.
	requireClosed(t, first, "the lost watcher must be closed before its replacement opens")
	requireClosed(t, firstPlumb, "the lost watcher's .plumb sibling must be closed before its replacement opens")
	// The reconcile reload: the workspace may have changed while blind.
	awaitDispatch(t, sig, root)
	if !m.healthy(ws) {
		t.Error("healthy = false after a successful recreate; the poll fallback would stay engaged")
	}

	// The replacement is live: an ordinary edit dispatches through it.
	writeProjectCfg(t, ws, "[edits]\nstrict = false\n")
	awaitDispatch(t, sig, root)

	m.close()
	requireClosed(t, second, "close must not return before the replacement watcher is closed")
	requireClosed(t, secondPlumb, "close must not return before the replacement .plumb watcher is closed")
}

// TestProjectWatchManager_LostAgainFallsBackToPoll pins the recreate budget:
// a replacement lost again straight away means the cause is persistent, so the
// loop stops (no third watcher, no spin) and the workspace is left to the poll
// fallback — failed and dead, the state acquire retries from.
func TestProjectWatchManager_LostAgainFallsBackToPoll(t *testing.T) {
	m, _ := testWatchManager(t, nil)
	m.recreateInterval = time.Hour // "straight away", however slow the runner
	built := captureWatchers(m)
	ws := watchedTempDir(t, m)
	m.acquire(ws)
	m.mu.Lock()
	w := m.watches[paths.Canonical(ws)]
	m.mu.Unlock()

	nextLoopWatchers(t, built)
	m.testErrs <- lostWatcherErr()
	second, _ := nextLoopWatchers(t, built)
	m.testErrs <- lostWatcherErr()

	// The loop gives up on its own and closes the replacement on the way out.
	select {
	case _, ok := <-second.Events:
		if ok {
			t.Fatal("replacement watcher still delivering after being lost again")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("loop kept the twice-lost watcher open; it must give up and close it")
	}
	if m.healthy(ws) {
		t.Error("healthy = true after the watcher was lost twice; the poll fallback must own the workspace")
	}
	m.close()
	if !w.dead.Load() || !w.failed.Load() {
		t.Errorf("dead=%v failed=%v, want both — acquire retries only a failed, exited watch", w.dead.Load(), w.failed.Load())
	}
	select {
	case <-built:
		t.Error("a third pair of watchers was built; a watcher lost twice in a row must not be recreated again")
	default:
	}
}

// TestProjectWatchManager_OtherErrorKeepsLoop keeps the long-standing
// contract for an error that is NOT a lost descriptor: mark failed (the poll
// re-engages) but keep the same watcher and loop running.
func TestProjectWatchManager_OtherErrorKeepsLoop(t *testing.T) {
	m, sig := testWatchManager(t, nil)
	built := captureWatchers(m)
	ws := watchedTempDir(t, m)
	writeProjectCfg(t, ws, "")
	m.acquire(ws)
	nextLoopWatchers(t, built)
	m.testErrs <- fsnotify.ErrEventOverflow

	writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	awaitDispatch(t, sig, paths.Canonical(ws))
	if m.healthy(ws) {
		t.Error("healthy = true after a watcher error; the poll fallback must re-engage")
	}
	select {
	case <-built:
		t.Error("a non-descriptor error rebuilt the watcher")
	default:
	}
}

// TestProjectWatchManager_AttachRetriesLostDescriptor: an attach that fails
// with EBADF is a stolen descriptor, not a bad workspace — one fresh attempt
// must recover it instead of leaving the workspace to the poll.
func TestProjectWatchManager_AttachRetriesLostDescriptor(t *testing.T) {
	m, _ := testWatchManager(t, nil)
	calls := 0
	m.newWatcher = func() (*fsnotify.Watcher, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("kqueue: %w", syscall.EBADF)
		}
		return fsnotify.NewWatcher()
	}
	ws := watchedTempDir(t, m)
	m.acquire(ws)
	if !m.healthy(ws) {
		t.Fatal("attach that lost a descriptor once was not retried")
	}
	if calls != 3 {
		t.Errorf("watcher builds = %d, want 3 (one lost, then the retry's root and .plumb watchers)", calls)
	}
}

// TestProjectWatchManager_CloseWaitsForReleasedWatcher is the perpetrator
// side. close used to return while a released watch goroutine was still
// closing its watcher, so a test's temp-dir clean-up deleted the watched tree
// DURING fsnotify's Close — the race that double-closes a descriptor. close
// must now wait out every goroutine, released ones included. The dispatch is
// held open so the old behaviour fails deterministically, not by timing luck.
func TestProjectWatchManager_CloseWaitsForReleasedWatcher(t *testing.T) {
	ws := t.TempDir() // before the manager, so the manager's clean-up runs first
	entered := make(chan struct{}, 1)
	unblock := make(chan struct{})
	finishDispatch := sync.OnceFunc(func() { close(unblock) })
	m := newProjectConfigWatchManager(context.Background(), func(string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-unblock
	})
	m.debounce = 50 * time.Millisecond
	// This test holds a dispatch open for longer than the production grace and
	// asserts on the wait itself, so it must not be cut short by the bound.
	m.closeGrace = testCloseGrace
	// Last registered runs first: release the held dispatch (never strand the
	// goroutine if the test fails early), then close the manager, and only
	// then does ws go.
	t.Cleanup(m.close)
	t.Cleanup(finishDispatch)
	built := captureWatchers(m)
	writeProjectCfg(t, ws, "")
	m.acquire(ws)
	watcher, plumbWatcher := nextLoopWatchers(t, built)

	writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no dispatch within 10s")
	}
	m.release(ws) // the last reference: the goroutine is cancelled but busy

	closed := make(chan struct{})
	go func() {
		m.close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("close returned while a released watch goroutine still held its watcher")
	case <-time.After(200 * time.Millisecond):
	}
	finishDispatch()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return after the dispatch finished")
	}
	requireClosed(t, watcher, "close returned before the released watcher was closed")
	requireClosed(t, plumbWatcher, "close returned before the released .plumb watcher was closed")
}

// TestProjectWatchManager_CloseIsBoundedWhenADispatchIsStuck: close waits for
// every watch goroutine, but one inside dispatch can be queued on a session's
// mutation lane behind slow work, and close runs on the orderly daemon
// shutdown path, where every step must be bounded
// (TestShutdownHardDeadlineExceedsInnerGraces). With a dispatch held open,
// close must give up at its grace — not before it, not never — and say so in
// the log; the abandoned goroutine must still finish once released. Must not
// run in parallel: it captures slog.Default.
func TestProjectWatchManager_CloseIsBoundedWhenADispatchIsStuck(t *testing.T) {
	warns := captureShutdownWarns(t)
	ws := t.TempDir() // before the manager, so the manager's clean-up runs first
	entered := make(chan struct{}, 1)
	unblock := make(chan struct{})
	finishDispatch := sync.OnceFunc(func() { close(unblock) })
	m := newProjectConfigWatchManager(context.Background(), func(string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-unblock
	})
	m.debounce = 50 * time.Millisecond
	const grace = 150 * time.Millisecond
	m.closeGrace = grace
	t.Cleanup(m.close)
	t.Cleanup(finishDispatch) // runs first: unstick the goroutine so m.close can finish
	writeProjectCfg(t, ws, "")
	m.acquire(ws)

	writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no dispatch within 10s")
	}

	took := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		m.close()
		took <- time.Since(start)
	}()
	select {
	case d := <-took:
		if d < grace {
			t.Errorf("close returned after %s, before its %s grace: it did not wait for the stuck goroutine", d, grace)
		}
	case <-time.After(grace + 5*time.Second):
		t.Fatal("close did not return within its grace while a watch goroutine was stuck in dispatch: the shutdown wait is unbounded")
	}
	if !warns.has("project config watchers") {
		t.Errorf("an abandoned close must log a warn naming the step; steps=%v", warns.snapshot())
	}

	// Released, the abandoned goroutine still runs to completion.
	finishDispatch()
	drained := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the abandoned watch goroutine did not finish once its dispatch was released")
	}
}

// TestCloseFSWatcher_WaitsForReader: once closeFSWatcher returns, the reader
// goroutine has stopped delivering (Events closed), so it can no longer react
// to a path deleted afterwards. Its final kqueue and close-pipe closes may still
// trail by microseconds, which this does not and cannot assert. A nil watcher
// is a no-op.
func TestCloseFSWatcher_WaitsForReader(t *testing.T) {
	closeFSWatcher(nil)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.Add(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	closeFSWatcher(watcher)
	requireClosed(t, watcher, "closeFSWatcher returned before the reader stopped delivering")
}

// TestGlobalConfigWatcher_RecreatesLostWatcher: the global watcher lives for
// the daemon's lifetime, so losing its descriptor used to end global hot
// reload for good (while its reader spun on EBADF). It must recreate in place
// and still reload; lost again at once, Run must give up with an error rather
// than spin.
func TestGlobalConfigWatcher_RecreatesLostWatcher(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	gw := newGlobalConfigWatcher(store)
	gw.debounce = 50 * time.Millisecond
	gw.recreateInterval = time.Hour // the second loss below counts as "straight away"
	gw.testErrs = make(chan error)
	built := make(chan *fsnotify.Watcher, 16)
	gw.newWatcher = func() (*fsnotify.Watcher, error) {
		w, err := fsnotify.NewWatcher()
		if err == nil {
			built <- w
		}
		return w, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx) }()
	// Deferred, not t.Cleanup: Run must have closed its watcher before the
	// temp dir it watches is removed.
	defer func() {
		cancel()
		<-done
	}()

	first := nextWatcher(t, built)
	gw.testErrs <- lostWatcherErr()
	second := nextWatcher(t, built)
	requireClosed(t, first, "the lost watcher must be closed before its replacement opens")

	if err := config.Save(func(c *config.Config) { c.Edits.Strict = true }); err != nil {
		t.Fatalf("Save: %v", err)
	}
	deadline := time.After(10 * time.Second)
	for !store.Current().Edits.Strict {
		select {
		case <-deadline:
			t.Fatal("the recreated global watcher did not reload strict=true within 10s")
		case <-time.After(20 * time.Millisecond):
		}
	}

	gw.testErrs <- lostWatcherErr()
	select {
	case err := <-done:
		done <- err // let the deferred clean-up's receive complete
		if !errors.Is(err, syscall.EBADF) {
			t.Errorf("Run returned %v, want the lost-again EBADF", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run kept going after its replacement watcher was lost straight away")
	}
	requireClosed(t, second, "Run returned without closing its watcher")
}
