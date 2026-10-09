//go:build linux || windows

package fswatch

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sgtdi/fswatcher"
)

// TestPumpSgtdi_LossAndMapping: an overflow marker and anything on sgtdi's
// Dropped channel must both signal Lost; ordinary events are mapped and
// delivered.
func TestPumpSgtdi_LossAndMapping(t *testing.T) {
	w := testWatcher("/r", 8)
	evs := make(chan fswatcher.WatchEvent)
	dropped := make(chan fswatcher.WatchEvent)
	finished := make(chan struct{})
	go func() { pumpSgtdi(w, evs, dropped); close(finished) }()

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
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the pump did not return when the Watcher closed")
	}
}

// TestPumpSgtdi_FullBufferSignalsLost: a consumer that stops reading costs a
// reconcile, not a blocked pump.
func TestPumpSgtdi_FullBufferSignalsLost(t *testing.T) {
	w := testWatcher("/r", 1)
	evs := make(chan fswatcher.WatchEvent)
	go pumpSgtdi(w, evs, nil)
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
	go pumpSgtdi(w, evs, nil)
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
