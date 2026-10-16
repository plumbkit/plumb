package topology

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/fswatch"
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
	fw, err := newFSWatcher(ws, sink, nil, false, nil)
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

// TestWatchOptions_LeaveLinkedWorktreesOutUnlessIndexed: the watcher the index
// shares with the language servers skips linked worktrees; the
// index_worktrees opt-in watches them on options of its own.
func TestWatchOptions_LeaveLinkedWorktreesOutUnlessIndexed(t *testing.T) {
	ws := t.TempDir()
	if !SharedWatchOptions(ws).SkipLinkedWorktrees || !watchOptions(ws, false).SkipLinkedWorktrees {
		t.Error("the shared watcher watches linked worktrees")
	}
	if watchOptions(ws, true).SkipLinkedWorktrees {
		t.Error("index_worktrees did not keep the worktrees' events")
	}
	if watchOptions(ws, false) != SharedWatchOptions(ws) {
		t.Error("the default index watcher does not share the language servers' options")
	}
}

// TestOpenWith_IndexWorktreesWatchesOnItsOwnOptions: a store that indexes
// worktrees does not join the shared watcher that leaves them out; one that
// does not, does.
func TestOpenWith_IndexWorktreesWatchesOnItsOwnOptions(t *testing.T) {
	for _, tc := range []struct {
		indexWorktrees bool
		watches        int
	}{{false, 1}, {true, 2}} {
		ws := t.TempDir()
		hub := fswatch.NewHub()
		s, err := OpenWith(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024, Watch: true, IndexWorktrees: tc.indexWorktrees}, nil, hub)
		if err != nil {
			t.Fatal(err)
		}
		lsp, err := hub.Subscribe(ws, SharedWatchOptions(ws), "")
		if err != nil {
			t.Fatal(err)
		}
		if got := hub.Watches(); got != tc.watches {
			t.Errorf("index_worktrees=%v: %d OS watchers beside the shared subscriber, want %d", tc.indexWorktrees, got, tc.watches)
		}
		lsp.Close()
		_ = s.Close()
	}
}
