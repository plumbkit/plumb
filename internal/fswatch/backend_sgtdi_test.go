//go:build linux || windows

package fswatch

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sgtdi/fswatcher"
)

// fakeSgtdi stands in for a running sgtdi watcher: only what the supervisor
// touches is implemented.
type fakeSgtdi struct {
	fswatcher.Watcher
	events, dropped chan fswatcher.WatchEvent
}

func (f *fakeSgtdi) Events() <-chan fswatcher.WatchEvent  { return f.events }
func (f *fakeSgtdi) Dropped() <-chan fswatcher.WatchEvent { return f.dropped }
func (f *fakeSgtdi) Close()                               {}

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
	finished := make(chan struct{})
	go func() { w.superviseSgtdi(Options{}, run); close(finished) }()
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

// runPump runs pumpSgtdi on its own goroutine and returns how it ended.
func runPump(w *Watcher, evs, dropped <-chan fswatcher.WatchEvent, stopped <-chan struct{}) <-chan runEnd {
	end := make(chan runEnd, 1)
	go func() { end <- pumpSgtdi(w, evs, dropped, stopped) }()
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
