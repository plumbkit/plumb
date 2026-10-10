//go:build linux

package fswatch

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// limitE2EEnv turns on TestWatcher_WatchLimitAfterStartIsDegraded, which
// needs fs.inotify.max_user_watches lowered first; scripts/test-watch-limit.sh
// does that, and CI's Linux verify leg runs it.
const limitE2EEnv = "PLUMB_FSWATCH_LIMIT_E2E"

// TestWatcher_WatchLimitAfterStartIsDegraded drives a real Watcher into the
// inotify watch limit after it has started. sgtdi discards the error when it
// cannot watch a directory created later, so without the probe the Watcher
// stayed complete in its own eyes while the new directories went unwatched.
func TestWatcher_WatchLimitAfterStartIsDegraded(t *testing.T) {
	if os.Getenv(limitE2EEnv) != "1" {
		t.Skip("needs a lowered fs.inotify.max_user_watches: run scripts/test-watch-limit.sh")
	}
	root := t.TempDir()
	spare := spareWatches(t, 5000)
	if spare < 20 {
		t.Fatalf("only %d inotify watches to spare; scripts/test-watch-limit.sh leaves more", spare)
	}
	// A tree that fits, the root's own watch included, with a few to spare.
	for i := range spare - 5 {
		if err := os.Mkdir(filepath.Join(root, "d"+strconv.Itoa(i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w, err := New(root, Options{Cooldown: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	collect(t, w)
	if isClosed(w.Failed()) {
		t.Fatal("a tree within the watch limit degraded the Watcher")
	}

	for i := range 10 {
		if err := os.Mkdir(filepath.Join(root, "new"+strconv.Itoa(i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-w.Failed():
	case <-time.After(15 * time.Second):
		t.Fatal("new directories went past the inotify watch limit and the Watcher did not say so")
	}
}

// spareWatches counts how many more inotify watches this user can add, up to
// most, by adding them on fresh directories through a throwaway instance,
// which closing releases.
func spareWatches(t *testing.T, most int) int {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	base := t.TempDir()
	for i := range most {
		dir := filepath.Join(base, strconv.Itoa(i))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_DELETE_SELF); err != nil {
			if errors.Is(err, unix.ENOSPC) {
				return i
			}
			t.Fatal(err)
		}
	}
	t.Fatalf("more than %d inotify watches to spare; lower fs.inotify.max_user_watches first", most)
	return 0
}
