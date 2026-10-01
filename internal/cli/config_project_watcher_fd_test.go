package cli

// config_project_watcher_fd_test.go — descriptor accounting for the project
// config watcher when a workspace's .plumb directory appears after attach
// (#568).

import (
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// openFDCount reports how many descriptors this process holds, by counting
// /dev/fd. Listing the directory opens one descriptor of its own, which every
// reading shares, so differences between readings are exact.
func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatalf("read /dev/fd: %v", err)
	}
	return len(entries)
}

// TestProjectWatchManager_PlumbDirChurnDoesNotLeakFDs pins #568. On kqueue
// (macOS and the BSDs) fsnotify watches every directory created inside a
// watched one, and the watch loop's own Add of a new .plumb ran at the same
// moment. The two race inside fsnotify, both open the directory, and when the
// directory goes away one of the descriptors is never closed: one leaked
// descriptor per .plumb created while a session is attached.
//
// Whether a given duplicate leaks depends on event timing, so the test checks
// the cause as well as the symptom: with .plumb present the process must hold
// no more than one descriptor for it (two means a second registration was made
// for a path that was already watched), and after 50 create/remove cycles the
// descriptor count must not have grown by anything near 50. Where descriptors
// are not per-watch (Linux inotify) both bounds hold trivially.
func TestProjectWatchManager_PlumbDirChurnDoesNotLeakFDs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/fd to count descriptors with")
	}
	const (
		cycles = 50
		// slack absorbs descriptors unrelated to the watcher opening or closing
		// meanwhile; a leak is about one per cycle.
		slack = 10
	)

	var dispatches atomic.Int64
	m := newProjectConfigWatchManager(t.Context(), func(string) { dispatches.Add(1) })
	m.debounce = 10 * time.Millisecond
	m.closeGrace = testCloseGrace
	ws := watchedTempDir(t, m)
	m.acquire(ws) // a pinned workspace root with no .plumb yet

	// await runs change, then waits for the watch loop to dispatch for it: the
	// loop dispatches only after it has handled the events that preceded the
	// dispatch, so once it returns the loop has finished reacting to change.
	await := func(change func()) {
		t.Helper()
		seen := dispatches.Load()
		change()
		deadline := time.Now().Add(10 * time.Second)
		for dispatches.Load() <= seen {
			if time.Now().After(deadline) {
				t.Fatal("no dispatch within 10s of a .plumb change")
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	mustDo := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	plumbDir := filepath.Join(ws, ".plumb")
	cfg := filepath.Join(plumbDir, "config.toml")
	before := openFDCount(t)
	for i := range cycles {
		await(func() { mustDo(os.Mkdir(plumbDir, 0o755)) })
		if held := openFDCount(t) - before; held > 1 {
			t.Fatalf("cycle %d: %d descriptors held for the new .plumb directory, want at most 1: it was registered twice", i, held)
		}
		await(func() { mustDo(os.WriteFile(cfg, []byte("[edits]\nstrict = true\n"), 0o644)) })
		await(func() { mustDo(os.RemoveAll(plumbDir)) })
	}

	// Descriptors a healthy watcher releases go as its reader handles the
	// removals, a moment after the loop catches up, so poll until the count
	// settles rather than sleeping. A leak is permanent: the poll only ever
	// delays a failure.
	var grown int
	for deadline := time.Now().Add(3 * time.Second); ; {
		grown = openFDCount(t) - before
		if grown <= slack || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if grown > slack {
		t.Fatalf("%d create/remove cycles of .plumb grew the open descriptor count by %d (slack %d): a descriptor leaks per cycle", cycles, grown, slack)
	}

	// Positive control: the attach still happens. A fix that simply never
	// watched .plumb would pass the counts above and go blind to config edits.
	await(func() { writeProjectCfg(t, ws, "[edits]\nstrict = false\n") })
	await(func() { writeProjectCfg(t, ws, "[edits]\nstrict = true\n") })
}
