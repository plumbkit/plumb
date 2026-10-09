package topology

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// TestFSWatcher_NewSymlinkIsEnqueuedUnderItsOwnName: the indexer keeps a
// symlink whose target lies inside the workspace under the link's own path, so
// the watcher must enqueue that path when the link appears. A backend that
// resolved event paths reported the target instead, and with periodic resync
// suppressed while watching, the alias stayed unindexed until a restart
// (PLAN-488 review).
func TestFSWatcher_NewSymlinkIsEnqueuedUnderItsOwnName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs privileges on Windows")
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ws, "payload.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	fw, err := newFSWatcher(ws, sink, nil)
	if err != nil {
		t.Fatalf("newFSWatcher: %v", err)
	}
	fw.Start()
	t.Cleanup(fw.Stop)
	enqueued := func(rel string) func() bool {
		return func() bool {
			enq, _ := sink.snapshot()
			return slices.Contains(enq, rel)
		}
	}
	warm := waitFor(func() bool {
		if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return waitFor(enqueued("main.go"), 500*time.Millisecond)
	}, 30*time.Second)
	if !warm {
		t.Fatal("the watcher delivered no event at all")
	}

	if err := os.Symlink(target, filepath.Join(ws, "alias.go")); err != nil {
		t.Fatal(err)
	}
	if !waitFor(enqueued("alias.go"), 15*time.Second) {
		enq, _ := sink.snapshot()
		t.Fatalf("a new in-workspace symlink was never enqueued under its own name; enqueued %v", enq)
	}
	if err := os.Rename(filepath.Join(ws, "alias.go"), filepath.Join(ws, "renamed_alias.go")); err != nil {
		t.Fatal(err)
	}
	if !waitFor(enqueued("renamed_alias.go"), 15*time.Second) {
		enq, _ := sink.snapshot()
		t.Errorf("a renamed symlink was never enqueued under its new name; enqueued %v", enq)
	}
}
