//go:build linux || windows

package fswatch

import (
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgtdi/fswatcher"
)

// hungRun is a run whose initial walk never ends, as on a stalled network
// mount: it never becomes ready, and cancelling it does not stop it, because
// sgtdi cannot interrupt its walk. cancelled reports whether it was told to.
func hungRun() (run *sgtdiRun, cancelled *atomic.Bool) {
	cancelled = new(atomic.Bool)
	return &sgtdiRun{
		src:     &fakeSgtdi{events: make(chan fswatcher.WatchEvent), dropped: make(chan fswatcher.WatchEvent)},
		cancel:  func() { cancelled.Store(true) },
		ready:   make(chan struct{}),
		stopped: make(chan struct{}),
	}, cancelled
}

// hungStarts returns a startFunc that starts run every time, and a channel
// that receives a value per start.
func hungStarts(run *sgtdiRun) (startFunc, <-chan struct{}) {
	started := make(chan struct{}, 8)
	return func(string, Options) (*sgtdiRun, error) {
		started <- struct{}{}
		return run, nil
	}, started
}

func awaitStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no restart was attempted")
	}
}

// TestSuperviseSgtdi_RestartWalkThatHangsIsDegraded: a root that came back on a
// stalled mount starts a run whose walk never finishes. Within the bound, the
// Watcher is degraded (Failed and Lost) instead of staying silently blind, and
// the hung run is abandoned: the supervisor does not wait for it.
func TestSuperviseSgtdi_RestartWalkThatHangsIsDegraded(t *testing.T) {
	w := testWatcher(t.TempDir(), 8)
	hung, cancelled := hungRun()
	start, started := hungStarts(hung)
	first := fakeRun()
	finished := superviseWithin(t, w, first, start, 200*time.Millisecond)
	loseRoot(t, w, first)
	awaitStart(t, started)

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor waited on a restart walk that never finished")
	}
	if !isClosed(w.failed) {
		t.Error("a restart walk that never finished did not close Failed")
	}
	if !lostSignalled(w) {
		t.Error("a restart walk that never finished did not signal Lost")
	}
	if !cancelled.Load() {
		t.Error("the hung run was not told to stop")
	}
}

// TestSuperviseSgtdi_CloseDuringAHungRestartWalk: Close does not wait for a
// restart walk that hangs. It used to wait until the walk ended, which on a
// stalled mount may be never.
func TestSuperviseSgtdi_CloseDuringAHungRestartWalk(t *testing.T) {
	w := testWatcher(t.TempDir(), 8)
	hung, cancelled := hungRun()
	start, started := hungStarts(hung)
	first := fakeRun()
	finished := supervise(t, w, first, start) // a minute's bound: Close comes first
	loseRoot(t, w, first)
	awaitStart(t, started)

	close(w.done)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the Watcher waited on a restart walk that never finished")
	}
	if !cancelled.Load() {
		t.Error("the hung run was not told to stop")
	}
	if isClosed(w.failed) {
		t.Error("closing the Watcher marked it failed")
	}
}

// TestSuperviseSgtdi_CloseDuringAHungWalkOfANewDirectory: sgtdi walks a
// directory that arrives inside its own event loop, which a stalled mount can
// hang, and cancelling the run does not interrupt that walk. Close does not
// wait for it either.
func TestSuperviseSgtdi_CloseDuringAHungWalkOfANewDirectory(t *testing.T) {
	w := testWatcher(t.TempDir(), 8)
	run, cancelled := hungRun()
	close(run.ready) // the run started fine; it is a later walk that hangs
	start, _ := fakeStarts()
	finished := supervise(t, w, run, start)

	close(w.done)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the Watcher waited on a run whose walk never ended")
	}
	if !cancelled.Load() {
		t.Error("the run was not told to stop")
	}
}

// TestSuperviseSgtdi_CloseWaitsForANormalStop: a run that stops promptly, as
// sgtdi does when it is not mid-walk, is waited for. Otherwise a watcher
// started right after on the same tree (LSP hibernation, reconfiguration)
// overlaps the old one's inotify watches, which near the limit can leave the
// new walk partial and the new Watcher degraded for good.
func TestSuperviseSgtdi_CloseWaitsForANormalStop(t *testing.T) {
	w := testWatcher(t.TempDir(), 8)
	run := fakeRun()
	stopNow := run.cancel
	run.cancel = func() {
		go func() {
			time.Sleep(50 * time.Millisecond)
			stopNow()
		}()
	}
	start, _ := fakeStarts()
	finished := supervise(t, w, run, start)

	close(w.done)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not return after the Watcher closed")
	}
	if !isClosed(run.stopped) {
		t.Error("Close returned before a run that stops promptly had stopped")
	}
}

// TestPumpSgtdi_NewDirectoryAtTheWatchLimitIsDegraded: sgtdi discards the
// error when it cannot watch a directory that arrives later, at the inotify
// watch limit. The pump checks each new directory, so the Watcher says it can
// no longer see every change: Failed, once, and it keeps delivering. A file is
// never checked, nor is anything once the Watcher is degraded.
func TestPumpSgtdi_NewDirectoryAtTheWatchLimitIsDegraded(t *testing.T) {
	root := t.TempDir()
	populate(t, root, "fine/x.go", "full/y.go", "later/z.go", "file.go")
	w := testWatcher(root, 64)
	var mu sync.Mutex
	var probed []string
	limitReached := func(dir string) bool {
		mu.Lock()
		defer mu.Unlock()
		probed = append(probed, dir)
		return filepath.Base(dir) == "full"
	}
	evs := make(chan fswatcher.WatchEvent)
	runPumpLimited(w, evs, nil, nil, limitReached)
	t.Cleanup(func() { close(w.done) })
	create := func(rel string) {
		evs <- fswatcher.WatchEvent{Path: filepath.Join(root, rel), Types: []fswatcher.EventType{fswatcher.EventCreate}}
	}
	// handled sends rel and then a file event. The pump handles one event at a
	// time, so the second send returns only once rel has been handled in full.
	handled := func(rel string) {
		create(rel)
		create("file.go")
	}

	handled("file.go")
	handled("fine")
	if isClosed(w.failed) {
		t.Fatal("a new directory under the limit degraded the Watcher")
	}
	handled("full")
	if !isClosed(w.failed) {
		t.Fatal("a new directory at the watch limit did not close Failed")
	}
	handled("later")

	mu.Lock()
	got := slices.Clone(probed)
	mu.Unlock()
	if want := []string{filepath.Join(root, "fine"), filepath.Join(root, "full")}; !slices.Equal(got, want) {
		t.Errorf("checked %v against the limit, want %v", got, want)
	}
	var delivered []Event
	for len(w.events) > 0 {
		delivered = append(delivered, <-w.events)
	}
	if !slices.ContainsFunc(delivered, func(ev Event) bool { return ev.Path == filepath.Join(root, "later", "z.go") }) {
		t.Errorf("a degraded Watcher stopped delivering: got %v", relPaths(root, delivered))
	}
}

// TestSuperviseSgtdi_NewDirectoryLeftPartialIsDegraded: sgtdi counts a
// subdirectory of a new directory that it could not watch at the limit
// (EventsPartial), as it does in its initial walk. That count, too, degrades
// the Watcher when the new directory arrives, with no probe needed.
func TestSuperviseSgtdi_NewDirectoryLeftPartialIsDegraded(t *testing.T) {
	root := t.TempDir()
	populate(t, root, "whole/a.go", "torn/sub/b.go", "file.go")
	w := testWatcher(root, 32)
	start, _ := fakeStarts()
	first := fakeRun() // no probe: only sgtdi's own count can say
	supervise(t, w, first, start)
	src := first.src.(*fakeSgtdi)
	create := func(rel string) {
		src.events <- fswatcher.WatchEvent{Path: filepath.Join(root, rel), Types: []fswatcher.EventType{fswatcher.EventCreate}}
	}

	create("whole")
	create("file.go")
	if isClosed(w.failed) {
		t.Fatal("a new directory watched in full degraded the Watcher")
	}
	src.partial.Store(1) // sgtdi could not watch torn/sub
	create("torn")
	create("file.go")
	if !isClosed(w.failed) {
		t.Fatal("a new directory left partly unwatched did not close Failed")
	}
}
