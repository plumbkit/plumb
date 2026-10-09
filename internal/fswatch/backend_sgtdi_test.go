//go:build linux || windows

package fswatch

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgtdi/fswatcher"
)

// fakeSgtdi stands in for a running sgtdi watcher: only what the supervisor
// touches is implemented.
type fakeSgtdi struct {
	fswatcher.Watcher
	events, dropped chan fswatcher.WatchEvent
	partial         atomic.Int64 // directories the walk could not watch
}

func (f *fakeSgtdi) Events() <-chan fswatcher.WatchEvent  { return f.events }
func (f *fakeSgtdi) Dropped() <-chan fswatcher.WatchEvent { return f.dropped }
func (f *fakeSgtdi) Close()                               {}
func (f *fakeSgtdi) Stats() fswatcher.WatcherStats {
	return fswatcher.WatcherStats{EventsPartial: f.partial.Load()}
}

// fakeRun is a run whose readiness the test controls. Cancelling it (as close
// does) stops it, as cancelling sgtdi's Watch does.
func fakeRun() *sgtdiRun {
	stopped := make(chan struct{})
	var once sync.Once
	return &sgtdiRun{
		src:     &fakeSgtdi{events: make(chan fswatcher.WatchEvent), dropped: make(chan fswatcher.WatchEvent)},
		cancel:  func() { once.Do(func() { close(stopped) }) },
		ready:   make(chan struct{}),
		stopped: stopped,
	}
}

// fakeStarts returns a startFunc that hands every run it starts to the test.
func fakeStarts() (startFunc, <-chan *sgtdiRun) {
	runs := make(chan *sgtdiRun, 8)
	return func(string, Options) (*sgtdiRun, error) {
		r := fakeRun()
		runs <- r
		return r, nil
	}, runs
}

func nextRun(t *testing.T, runs <-chan *sgtdiRun, what string) *sgtdiRun {
	t.Helper()
	select {
	case r := <-runs:
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no run was started", what)
		return nil
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func awaitLost(t *testing.T, w *Watcher, what string) {
	t.Helper()
	select {
	case <-w.lost:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: Lost was not signalled", what)
	}
}

// notBeforeReady is how long a test watches for something that must not happen
// while a run is not ready. Without the readiness gate the supervisor acts
// within microseconds of a start returning, so this is generous.
const notBeforeReady = 300 * time.Millisecond

// supervise runs the supervisor over first and returns a channel closed when it
// returns. The Watcher is closed at cleanup if the test has not closed it, and
// a supervisor that then fails to return is reported rather than waited on.
func supervise(t *testing.T, w *Watcher, first *sgtdiRun, start startFunc) <-chan struct{} {
	t.Helper()
	return superviseWithin(t, w, first, start, readyWithin)
}

// superviseWithin is supervise with the bound on a restart's walk.
func superviseWithin(t *testing.T, w *Watcher, first *sgtdiRun, start startFunc, within time.Duration) <-chan struct{} {
	t.Helper()
	finished := make(chan struct{})
	go func() { w.superviseSgtdi(Options{}, first, start, within); close(finished) }()
	t.Cleanup(func() {
		if !isClosed(w.done) {
			close(w.done)
		}
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("the supervisor did not return after the Watcher closed")
		}
	})
	return finished
}

// loseRoot ends first's run as a lost root and takes the Lost it signals.
func loseRoot(t *testing.T, w *Watcher, first *sgtdiRun) {
	t.Helper()
	first.src.(*fakeSgtdi).events <- fswatcher.WatchEvent{Path: w.root, Types: []fswatcher.EventType{fswatcher.EventRename}}
	awaitLost(t, w, "root renamed away")
}

// TestSuperviseSgtdi_FailureIsDegraded: a backend that stops on its own with the
// root still in place is not restarted (it would most likely fail again, and
// each restart costs a reconcile); the Watcher reports Failed, for good, and
// Lost for what was missed.
func TestSuperviseSgtdi_FailureIsDegraded(t *testing.T) {
	root := t.TempDir()
	w := testWatcher(root, 8)
	stopped := make(chan struct{})
	close(stopped)
	run := &sgtdiRun{
		src:     &fakeSgtdi{events: make(chan fswatcher.WatchEvent), dropped: make(chan fswatcher.WatchEvent)},
		cancel:  func() {},
		stopped: stopped,
	}
	restarted := func(string, Options) (*sgtdiRun, error) {
		t.Error("a failed backend was restarted")
		return nil, errors.New("no restart")
	}
	finished := make(chan struct{})
	go func() { w.superviseSgtdi(Options{}, run, restarted, readyWithin); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not give up on a failed backend")
	}
	select {
	case <-w.Failed():
	default:
		t.Error("a failed backend did not close Failed")
	}
	if !lostSignalled(w) {
		t.Error("a failed backend did not signal Lost")
	}
}

// TestStartSupervised_WaitsForReadiness: New returns only once the backend's
// initial watches are in place, so a consumer's first full scan cannot finish
// before a change it misses is watched.
func TestStartSupervised_WaitsForReadiness(t *testing.T) {
	w := testWatcher(t.TempDir(), 8)
	start, runs := fakeStarts()
	returned := make(chan error, 1)
	go func() { returned <- w.startSupervised(Options{}, start, time.Minute) }()
	run := nextRun(t, runs, "New")
	select {
	case err := <-returned:
		t.Fatalf("New returned (err %v) before the backend's watches were in place", err)
	case <-time.After(notBeforeReady):
	}
	close(run.ready)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("New: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("New did not return once the backend was ready")
	}
	close(w.done)
	w.wg.Wait()
	if !isClosed(run.stopped) {
		t.Error("closing the Watcher left the run going")
	}
}

// TestStartSupervised_StoppedBeforeReadyFails: a backend that cannot put its
// watches in place fails New, with its own error, and is not left running.
func TestStartSupervised_StoppedBeforeReadyFails(t *testing.T) {
	w := testWatcher(t.TempDir(), 8)
	cause := errors.New("inotify: no space left on device")
	run := fakeRun()
	run.err = cause
	run.cancel()
	err := w.startSupervised(Options{}, func(string, Options) (*sgtdiRun, error) { return run, nil }, time.Minute)
	if !errors.Is(err, cause) {
		t.Fatalf("New = %v, want the backend's own error", err)
	}
}

// TestStartSupervised_WalkThatHangsFailsNew: a walk that never finishes (a
// stalled network mount, say) fails New within the bound instead of hanging
// the consumer's start, and the run is cancelled.
func TestStartSupervised_WalkThatHangsFailsNew(t *testing.T) {
	w := testWatcher(t.TempDir(), 8)
	run := fakeRun()
	returned := make(chan error, 1)
	go func() {
		returned <- w.startSupervised(Options{}, func(string, Options) (*sgtdiRun, error) { return run, nil }, 100*time.Millisecond)
	}()
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("New succeeded although the backend never became ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("New hung on a walk that never finished")
	}
	if !isClosed(run.stopped) {
		t.Error("the run New gave up on was not cancelled")
	}
}

// TestSuperviseSgtdi_PartialWalkIsDegraded: sgtdi skips a directory it cannot
// watch at the inotify watch limit and still reports ready. That Watcher can no
// longer see every change, so it reports Failed and Lost, and keeps delivering
// what it does watch. A later failure must not close Failed a second time.
func TestSuperviseSgtdi_PartialWalkIsDegraded(t *testing.T) {
	root := t.TempDir()
	w := testWatcher(root, 8)
	start, _ := fakeStarts()
	first := fakeRun()
	first.src.(*fakeSgtdi).partial.Store(3)
	finished := supervise(t, w, first, start)

	awaitLost(t, w, "a partial walk")
	if !isClosed(w.failed) {
		t.Fatal("a partial walk did not close Failed")
	}
	first.src.(*fakeSgtdi).events <- fswatcher.WatchEvent{Path: filepath.Join(root, "a.go"), Types: []fswatcher.EventType{fswatcher.EventMod}}
	select {
	case <-w.events:
	case <-time.After(5 * time.Second):
		t.Fatal("a degraded Watcher stopped delivering what it does watch")
	}

	first.err = errors.New("inotify read failed")
	first.cancel() // the run then stops on its own, with the root in place
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not return after the run failed")
	}
}

// TestSuperviseSgtdi_PartialRestartIsDegraded: the same holds for a run started
// after the root came back.
func TestSuperviseSgtdi_PartialRestartIsDegraded(t *testing.T) {
	w := testWatcher(t.TempDir(), 8)
	start, runs := fakeStarts()
	first := fakeRun()
	supervise(t, w, first, start)
	loseRoot(t, w, first)
	second := nextRun(t, runs, "restart")
	second.src.(*fakeSgtdi).partial.Store(1)
	close(second.ready)
	awaitLost(t, w, "the partial restart became ready")
	deadline := time.After(5 * time.Second)
	for !isClosed(w.failed) {
		select {
		case <-deadline:
			t.Fatal("a partial restart did not close Failed")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestSuperviseSgtdi_RestartWaitsForReadiness: after a lost root, the reconcile
// that covers what was made while the root was unwatched is asked for only once
// the new run's watches are in place. Asked earlier, a consumer could finish
// reconciling before a watch existed and miss a change made in between for good.
func TestSuperviseSgtdi_RestartWaitsForReadiness(t *testing.T) {
	root := t.TempDir()
	w := testWatcher(root, 8)
	start, runs := fakeStarts()
	first := fakeRun()
	supervise(t, w, first, start)
	loseRoot(t, w, first) // the root is still a directory: recreated, say

	second := nextRun(t, runs, "restart")
	select {
	case <-w.lost:
		t.Fatal("the reconcile after a restart was asked for before the new run's watches were in place")
	case <-time.After(notBeforeReady):
	}
	close(second.ready)
	awaitLost(t, w, "the new run became ready")

	second.src.(*fakeSgtdi).events <- fswatcher.WatchEvent{Path: filepath.Join(root, "a.go"), Types: []fswatcher.EventType{fswatcher.EventCreate}}
	select {
	case ev := <-w.events:
		if ev.Path != filepath.Join(root, "a.go") {
			t.Errorf("delivered %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the restarted run's events are not pumped")
	}
	if isClosed(w.failed) {
		t.Error("a lost and recovered root marked the Watcher failed")
	}
}

// TestSuperviseSgtdi_CloseWhileRestartIsPending: Close does not wait for a
// pending run to become ready; it stops the run, and nothing is reported for it.
// (A real sgtdi run returns only once its initial walk ends, which cannot be
// interrupted; this fake returns as soon as it is cancelled.)
func TestSuperviseSgtdi_CloseWhileRestartIsPending(t *testing.T) {
	w := testWatcher(t.TempDir(), 8)
	start, runs := fakeStarts()
	first := fakeRun()
	finished := supervise(t, w, first, start)
	loseRoot(t, w, first)
	second := nextRun(t, runs, "restart")

	close(w.done)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the Watcher waited on a run that never became ready")
	}
	if !isClosed(second.stopped) {
		t.Error("the pending run was left going")
	}
	if lostSignalled(w) {
		t.Error("Lost was signalled for a run that never became ready")
	}
	if isClosed(w.failed) {
		t.Error("closing the Watcher marked it failed")
	}
}

// TestSuperviseSgtdi_RestartThatStopsBeforeReadyIsDegraded: a restarted run
// that stops before it is ready, with the root still in place, is a failure
// like any other: Failed, and Lost, and no restart loop.
func TestSuperviseSgtdi_RestartThatStopsBeforeReadyIsDegraded(t *testing.T) {
	w := testWatcher(t.TempDir(), 8)
	start, runs := fakeStarts()
	first := fakeRun()
	finished := supervise(t, w, first, start)
	loseRoot(t, w, first)
	second := nextRun(t, runs, "restart")
	second.err = errors.New("inotify: no space left on device")
	second.cancel()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not give up on a restart that failed")
	}
	if !isClosed(w.failed) {
		t.Error("a restart that failed did not close Failed")
	}
	if !lostSignalled(w) {
		t.Error("a restart that failed did not signal Lost")
	}
	select {
	case r := <-runs:
		t.Errorf("the supervisor started another run after a failure: %p", r)
	default:
	}
}

// TestSuperviseSgtdi_RootGoneAgainBeforeReadyIsWaitedFor: a restarted run that
// stops because the root went again is another lost root, not a failure: the
// supervisor waits for the root once more and asks for the reconcile only when
// a run is ready.
func TestSuperviseSgtdi_RootGoneAgainBeforeReadyIsWaitedFor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	w := testWatcher(root, 8)
	start, runs := fakeStarts()
	first := fakeRun()
	supervise(t, w, first, start)
	loseRoot(t, w, first)

	second := nextRun(t, runs, "restart")
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	second.cancel()
	select {
	case r := <-runs:
		t.Fatalf("a run was started while the root was missing: %p", r)
	case <-time.After(2 * rootRetryMin):
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	third := nextRun(t, runs, "second restart")
	if lostSignalled(w) {
		t.Fatal("Lost was signalled before a restarted run was ready")
	}
	close(third.ready)
	awaitLost(t, w, "the third run became ready")
	if isClosed(w.failed) {
		t.Error("a root that came back marked the Watcher failed")
	}
	if !isClosed(second.stopped) {
		t.Error("the run that stopped before ready was not closed")
	}
}

// runPump runs pumpSgtdi on its own goroutine, never at the watch limit, and
// returns how it ended.
func runPump(w *Watcher, evs, dropped <-chan fswatcher.WatchEvent, stopped <-chan struct{}) <-chan runEnd {
	return runPumpLimited(w, evs, dropped, stopped, func(string) bool { return false })
}

// runPumpLimited is runPump with the watch-limit check the test gives it.
func runPumpLimited(w *Watcher, evs, dropped <-chan fswatcher.WatchEvent, stopped <-chan struct{}, limitReached func(string) bool) <-chan runEnd {
	end := make(chan runEnd, 1)
	go func() { end <- pumpSgtdi(w, evs, dropped, stopped, limitReached) }()
	return end
}

func awaitEnd(t *testing.T, end <-chan runEnd, want runEnd, what string) {
	t.Helper()
	select {
	case got := <-end:
		if got != want {
			t.Errorf("%s: the run ended %d, want %d", what, got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the pump did not return", what)
	}
}

// TestPumpSgtdi_LossAndMapping: an overflow marker and anything on sgtdi's
// Dropped channel must both signal Lost; ordinary events are mapped and
// delivered; closing the Watcher ends the run as closed.
func TestPumpSgtdi_LossAndMapping(t *testing.T) {
	w := testWatcher("/r", 8)
	evs := make(chan fswatcher.WatchEvent)
	dropped := make(chan fswatcher.WatchEvent)
	end := runPump(w, evs, dropped, nil)

	evs <- fswatcher.WatchEvent{Path: "/r/a.go", Types: []fswatcher.EventType{fswatcher.EventCreate, fswatcher.EventMod}}
	select {
	case ev := <-w.events:
		if ev != (Event{Path: "/r/a.go", Op: Create | Write}) {
			t.Errorf("mapped event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an ordinary event was not delivered")
	}

	evs <- fswatcher.WatchEvent{Path: "/r", Types: []fswatcher.EventType{fswatcher.EventOverflow}}
	select {
	case <-w.lost:
	case <-time.After(5 * time.Second):
		t.Fatal("an overflow marker did not signal Lost")
	}

	dropped <- fswatcher.WatchEvent{Path: "/r/b.go", Types: []fswatcher.EventType{fswatcher.EventMod}}
	select {
	case <-w.lost:
	case <-time.After(5 * time.Second):
		t.Fatal("a dropped event did not signal Lost")
	}

	close(w.done)
	awaitEnd(t, end, endClosed, "Watcher closed")
}

// TestPumpSgtdi_FullBufferSignalsLost: a consumer that stops reading costs a
// reconcile, not a blocked pump.
func TestPumpSgtdi_FullBufferSignalsLost(t *testing.T) {
	w := testWatcher("/r", 1)
	evs := make(chan fswatcher.WatchEvent)
	runPump(w, evs, nil, nil)
	t.Cleanup(func() { close(w.done) })
	for range 10 {
		select {
		case evs <- fswatcher.WatchEvent{Path: "/r/a.go", Types: []fswatcher.EventType{fswatcher.EventMod}}:
		case <-time.After(5 * time.Second):
			t.Fatal("the pump blocked on a consumer that is not reading")
		}
	}
	select {
	case <-w.lost:
	case <-time.After(5 * time.Second):
		t.Fatal("overflowing the buffer did not signal Lost")
	}
}

// TestPumpSgtdi_RootEventsEndTheRun: the root removed or renamed (even if a
// directory is already back in its place) ends the run as a lost root; the
// root's own metadata changes are neither delivered nor fatal.
func TestPumpSgtdi_RootEventsEndTheRun(t *testing.T) {
	root := t.TempDir()
	for _, typ := range []fswatcher.EventType{fswatcher.EventRename, fswatcher.EventRemove} {
		w := testWatcher(root, 8)
		evs := make(chan fswatcher.WatchEvent)
		end := runPump(w, evs, nil, nil)
		evs <- fswatcher.WatchEvent{Path: root, Types: []fswatcher.EventType{fswatcher.EventChmod}}
		evs <- fswatcher.WatchEvent{Path: root, Types: []fswatcher.EventType{typ}}
		awaitEnd(t, end, endRootLost, "root "+typ.String())
		if got := drain(w); len(got) != 0 {
			t.Errorf("root events were delivered: %v", got)
		}
	}
}

// TestPumpSgtdi_StoppedRun: a run whose Watch returned on its own is a lost
// root when the root is gone, and a failure, reported once and not restarted,
// when it is still there.
func TestPumpSgtdi_StoppedRun(t *testing.T) {
	root := t.TempDir()
	stopped := make(chan struct{})
	close(stopped)
	awaitEnd(t, runPump(testWatcher(root, 8), nil, nil, stopped), endFailed, "stopped, root present")
	gone := filepath.Join(root, "gone")
	awaitEnd(t, runPump(testWatcher(gone, 8), nil, nil, stopped), endRootLost, "stopped, root missing")
}

func TestOpFromTypes(t *testing.T) {
	cases := []struct {
		types []fswatcher.EventType
		want  Op
	}{
		{[]fswatcher.EventType{fswatcher.EventCreate}, Create},
		{[]fswatcher.EventType{fswatcher.EventMod}, Write},
		{[]fswatcher.EventType{fswatcher.EventRemove}, Remove},
		{[]fswatcher.EventType{fswatcher.EventRename}, Rename},
		{[]fswatcher.EventType{fswatcher.EventChmod}, Chmod},
		{[]fswatcher.EventType{fswatcher.EventUnknown}, Write},
		{[]fswatcher.EventType{fswatcher.EventCreate, fswatcher.EventRemove}, Create | Remove},
		{nil, 0},
	}
	for _, tc := range cases {
		if got := opFromTypes(tc.types); got != tc.want {
			t.Errorf("opFromTypes(%v) = %05b, want %05b", tc.types, got, tc.want)
		}
	}
}

// TestPumpSgtdi_ExpandsNewDirectory: inotify never reports a file created in a
// new directory before sgtdi watches it, so the pump reports the directory's
// contents itself.
func TestPumpSgtdi_ExpandsNewDirectory(t *testing.T) {
	root := t.TempDir()
	populate(t, filepath.Join(root, "a"), "b/c/new.go")
	w := testWatcher(root, 16)
	evs := make(chan fswatcher.WatchEvent)
	runPump(w, evs, nil, nil)
	t.Cleanup(func() { close(w.done) })
	evs <- fswatcher.WatchEvent{Path: filepath.Join(root, "a"), Types: []fswatcher.EventType{fswatcher.EventCreate}}
	var got []Event
	deadline := time.After(5 * time.Second)
	for len(got) < 4 {
		select {
		case ev := <-w.events:
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("got %v, want a, a/b, a/b/c and a/b/c/new.go", got)
		}
	}
	if rels := relPaths(root, got); !slices.Equal(rels, []string{"a", "a/b", "a/b/c", "a/b/c/new.go"}) {
		t.Errorf("got %v", rels)
	}
}
