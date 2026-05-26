package topology

import (
	"path/filepath"
	"testing"

	"github.com/sgtdi/fswatcher"
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
	ev := func(rel string, types ...fswatcher.EventType) fswatcher.WatchEvent {
		return fswatcher.WatchEvent{Path: filepath.Join(ws, filepath.FromSlash(rel)), Types: types}
	}
	tests := []struct {
		name        string
		ev          fswatcher.WatchEvent
		wantEnqueue string // "" means no enqueue expected
		wantResync  int
	}{
		{"modify enqueues", ev("internal/foo/bar.go", fswatcher.EventMod), filepath.FromSlash("internal/foo/bar.go"), 0},
		{"create enqueues", ev("main.go", fswatcher.EventCreate), "main.go", 0},
		{"remove enqueues (Enqueue routes to delete)", ev("old.go", fswatcher.EventRemove), "old.go", 0},
		{"under .plumb ignored (self-trigger guard)", ev(".plumb/topology.db", fswatcher.EventMod), "", 0},
		{"under node_modules ignored", ev("node_modules/pkg/index.js", fswatcher.EventMod), "", 0},
		{"under .git ignored", ev(".git/index", fswatcher.EventMod), "", 0},
		{"overflow escalates to resync", ev("anything.go", fswatcher.EventOverflow), "", 1},
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

// TestFSWatcher_StartStop exercises the real platform watcher's lifecycle: it
// must construct, start, and stop cleanly without hanging.
func TestFSWatcher_StartStop(t *testing.T) {
	fw, err := newFSWatcher(t.TempDir(), &fakeSink{})
	if err != nil {
		t.Fatalf("newFSWatcher: %v", err)
	}
	fw.Start()
	fw.Stop()
}
