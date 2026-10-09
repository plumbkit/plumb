package topology

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/fswatch"
)

// fakeSink records the watcher's actions so handle() can be tested without a
// real OS watcher or database.
type fakeSink struct {
	enqueued []string
	resyncs  int
}

func (f *fakeSink) Enqueue(path string) { f.enqueued = append(f.enqueued, path) }
func (f *fakeSink) Resync()             { f.resyncs++ }

func TestFSWatcher_Handle(t *testing.T) {
	ws := filepath.FromSlash("/work/repo")
	ev := func(rel string, op fswatch.Op) fswatch.Event {
		return fswatch.Event{Path: filepath.Join(ws, filepath.FromSlash(rel)), Op: op}
	}
	tests := []struct {
		name        string
		ev          fswatch.Event
		wantEnqueue string // "" means no enqueue expected
		wantResync  int
	}{
		{"modify enqueues", ev("internal/foo/bar.go", fswatch.Write), filepath.FromSlash("internal/foo/bar.go"), 0},
		{"create enqueues", ev("main.go", fswatch.Create), "main.go", 0},
		{"remove enqueues (Enqueue routes to delete)", ev("old.go", fswatch.Remove), "old.go", 0},
		{"rename enqueues", ev("moved.go", fswatch.Rename), "moved.go", 0},
		{"under .plumb ignored (self-trigger guard)", ev(".plumb/topology.db", fswatch.Write), "", 0},
		{"under node_modules ignored", ev("node_modules/pkg/index.js", fswatch.Write), "", 0},
		{"under .git ignored", ev(".git/index", fswatch.Write), "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &fakeSink{}
			fw := &fsWatcher{workspace: ws, sink: sink}
			fw.handle(tt.ev)
			if sink.resyncs != tt.wantResync {
				t.Errorf("resyncs = %d, want %d", sink.resyncs, tt.wantResync)
			}
			switch tt.wantEnqueue {
			case "":
				if len(sink.enqueued) != 0 {
					t.Errorf("enqueued = %v, want none", sink.enqueued)
				}
			default:
				if len(sink.enqueued) != 1 || sink.enqueued[0] != tt.wantEnqueue {
					t.Errorf("enqueued = %v, want [%q]", sink.enqueued, tt.wantEnqueue)
				}
			}
		})
	}
}

func TestShouldSkipPath(t *testing.T) {
	skip := []string{
		".plumb/topology.db", "node_modules/x", ".git/index", "vendor/x.go",
		"dist/a", "build/o", "__pycache__/m.pyc", ".venv/lib/x.py", "", ".", "../escape.go",
	}
	keep := []string{"main.go", "internal/foo/bar.go", "a/b/c.py", "cmd/plumb/main.go"}
	for _, p := range skip {
		if !shouldSkipPath(filepath.FromSlash(p)) {
			t.Errorf("shouldSkipPath(%q) = false, want true", p)
		}
	}
	for _, p := range keep {
		if shouldSkipPath(filepath.FromSlash(p)) {
			t.Errorf("shouldSkipPath(%q) = true, want false", p)
		}
	}
}

// TestFSWatcher_LostEscalatesToResync: a loss signal from the OS watcher (an
// overflow, a rescan request, a full buffer) must reconcile the whole tree,
// because the changes it stands for will never arrive as events. A closed loss
// channel ends the consumer rather than spinning.
func TestFSWatcher_LostEscalatesToResync(t *testing.T) {
	events := make(chan fswatch.Event)
	lost := make(chan struct{}, 1)
	sink := &lockedFakeSink{}
	fw := &fsWatcher{workspace: t.TempDir(), sink: sink, events: events, lost: lost, done: make(chan struct{})}
	fw.Start()
	lost <- struct{}{}
	if !waitFor(func() bool { return sink.resyncCount() == 1 }, 5*time.Second) {
		t.Fatalf("resyncs = %d after a loss signal, want 1", sink.resyncCount())
	}
	close(lost)
	done := make(chan struct{})
	go func() { fw.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the consumer did not return when the loss channel closed")
	}
	fw.Stop()
}

// TestFSWatcher_FailedFallsBackToPeriodicResync: a watcher that has failed for
// good delivers nothing more, and the store suppressed its own periodic resync
// while watching, so the consumer must reconcile on the configured interval
// from then on, and must not when that interval is 0.
func TestFSWatcher_FailedFallsBackToPeriodicResync(t *testing.T) {
	for _, tc := range []struct {
		name     string
		every    time.Duration
		wantMore bool
	}{
		{"configured interval", 20 * time.Millisecond, true},
		{"resync_interval_minutes = 0", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed := make(chan struct{})
			sink := &lockedFakeSink{}
			fw := &fsWatcher{
				workspace: t.TempDir(), sink: sink,
				events: make(chan fswatch.Event), lost: make(chan struct{}), failed: failed,
				fallbackEvery: tc.every, done: make(chan struct{}),
			}
			fw.Start()
			t.Cleanup(fw.Stop)
			time.Sleep(60 * time.Millisecond)
			if n := sink.resyncCount(); n != 0 {
				t.Fatalf("%d resyncs before the watcher failed", n)
			}
			close(failed)
			if tc.wantMore {
				if !waitFor(func() bool { return sink.resyncCount() >= 3 }, 5*time.Second) {
					t.Errorf("resyncs = %d after the watcher failed, want a steady fallback", sink.resyncCount())
				}
				return
			}
			time.Sleep(200 * time.Millisecond)
			if n := sink.resyncCount(); n != 0 {
				t.Errorf("%d resyncs with resync_interval_minutes = 0", n)
			}
		})
	}
}

// lockedFakeSink is a fakeSink safe to read while the consumer goroutine runs.
type lockedFakeSink struct {
	mu      sync.Mutex
	resyncs int
}

func (f *lockedFakeSink) Enqueue(string) {}
func (f *lockedFakeSink) Resync()        { f.mu.Lock(); f.resyncs++; f.mu.Unlock() }
func (f *lockedFakeSink) resyncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resyncs
}

// TestFSWatcher_StartStop exercises the real platform watcher's lifecycle: it
// must construct, start, and stop cleanly without hanging.
func TestFSWatcher_StartStop(t *testing.T) {
	fw, err := newFSWatcher(t.TempDir(), &fakeSink{}, nil)
	if err != nil {
		t.Fatalf("newFSWatcher: %v", err)
	}
	fw.Start()
	fw.Stop()
}
