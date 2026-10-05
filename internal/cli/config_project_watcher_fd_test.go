//go:build !windows

package cli

// config_project_watcher_fd_test.go — descriptor accounting for the project
// config watcher when a workspace's .plumb directory appears after attach
// (#568).

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/plumbkit/plumb/internal/paths"
)

// openFDCount reports how many descriptors this process holds, by counting
// the names in /dev/fd. Listing the directory opens one descriptor of its own,
// which every reading shares, so differences between readings are exact. It
// reads names only: os.ReadDir also stats each entry, and on macOS that fails
// with EBADF when another goroutine closes a descriptor between the listing and
// the stat (seen on CI, where earlier tests' watchers are still closing theirs).
func openFDCount(t *testing.T) int {
	t.Helper()
	var err error
	for range 5 {
		var dir *os.File
		if dir, err = os.Open("/dev/fd"); err != nil {
			continue
		}
		var names []string
		names, err = dir.Readdirnames(-1)
		_ = dir.Close()
		if err == nil {
			return len(names)
		}
	}
	t.Fatalf("read /dev/fd: %v", err)
	return 0
}

// descriptorsOn reports how many of this process's descriptors refer to the
// file at path, by fstat-ing each descriptor /dev/fd lists and comparing its
// device and inode with path's. (Statting the /dev/fd entries by name does not
// work: on macOS that describes the entry, not the open file.) Matching by file rather than counting all descriptors keeps
// the count blind to everything else the process opens meanwhile, such as the
// runtime's network poller, which Go starts on the first timer and which the
// watch loop's own debounce timer can therefore open mid-test. An entry that
// cannot be statted is skipped: it is the listing's own descriptor, or one
// another goroutine closed between the listing and the stat, and neither is
// path. A descriptor number reused for another file in that gap compares
// unequal, so it can only lower the count.
func descriptorsOn(t *testing.T, path string) int {
	t.Helper()
	var want syscall.Stat_t
	if err := syscall.Stat(path, &want); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatalf("open /dev/fd: %v", err)
	}
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil {
		t.Fatalf("read /dev/fd: %v", err)
	}
	n := 0
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) == nil && st.Dev == want.Dev && st.Ino == want.Ino {
			n++
		}
	}
	return n
}

// perWatchDescriptors reports whether fsnotify's backend here holds an open
// descriptor per watch (kqueue). Only there can the churn test see a watch by
// its descriptor, and only there does it need to.
var perWatchDescriptors = slices.Contains([]string{"darwin", "ios", "freebsd", "openbsd", "netbsd", "dragonfly"}, runtime.GOOS)

// attached reports whether the .plumb watcher holds a completed watch on the
// CURRENT plumbDir. Its WatchList alone cannot say: fsnotify keeps listing a
// user watch until its reader handles the directory's removal, so for a moment
// it still names the old, deleted directory. On kqueue the descriptor count
// settles it, provided the root watcher's reader has finished its own
// registration (rootRegistered), so that it holds exactly one descriptor on
// the new inode: a second is the .plumb watcher's. The count is read before
// WatchList on purpose. fsnotify records a user watch only after registering
// it, so a WatchList naming plumbDir, read after a second descriptor appeared,
// is the new watch and is complete, not the old one or one half made.
func attached(t *testing.T, plumbWatcher *fsnotify.Watcher, plumbDir string) bool {
	t.Helper()
	if perWatchDescriptors && descriptorsOn(t, plumbDir) < 2 {
		return false
	}
	return slices.Equal(plumbWatcher.WatchList(), []string{plumbDir})
}

// TestProjectWatchManager_PlumbDirChurnDoesNotLeakFDs pins #568. On kqueue
// (macOS and the BSDs) fsnotify watches every directory created inside a
// watched one, and the watch loop's own Add of a new .plumb, on that same
// watcher, ran at the same moment. The two race inside fsnotify, both open the
// directory, and when the directory goes away one of the descriptors is never
// closed: one leaked descriptor per .plumb created while a session is
// attached. The loop now adds .plumb on a watcher of its own, which
// TestProjectWatchManager_PlumbDirNeverSharesTheRootWatcher pins directly;
// this test is the symptom check, and catches the race only under load.
//
// With .plumb present the process may hold one descriptor for it per watcher:
// the root watcher's reader registers it, and the .plumb watcher holds the
// loop's Add. A third means one of them registered it twice. After 50
// create/remove cycles the descriptor count must not have grown by anything
// near 50. Where descriptors are not per-watch (Linux inotify) both bounds
// hold trivially.
//
// A path removed moments after it appears trips a different kqueue race in
// fsnotify (#595): the reader opens the new path and only then registers it,
// and a removal that lands between the two is never reported, so fsnotify
// keeps a dead watch that marks the name as seen and hides every later path of
// that name. That race is not this test's subject and plumb cannot close it,
// so the churn keeps out of it. It creates nothing inside .plumb, and checks
// the attach on the .plumb watcher instead of through a config write. And on
// kqueue it removes .plumb only once the root watcher's reader has finished
// registering it (rootRegistered): removed in that gap, .plumb is left as a
// dead watch in the root watcher, and the next .plumb is never reported.
func TestProjectWatchManager_PlumbDirChurnDoesNotLeakFDs(t *testing.T) {
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
	built := captureWatchers(m)
	ws := watchedTempDir(t, m)
	m.acquire(ws) // a pinned workspace root with no .plumb yet
	_, plumbWatcher := nextLoopWatchers(t, built)

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

	plumbDir := filepath.Join(paths.Canonical(ws), ".plumb")

	// rootRegistered waits until the root watcher's reader has finished
	// registering the .plumb that just appeared. That registration is not
	// observable directly, so it is ordered against one that is. The reader
	// handles kevents one at a time, and lists a directory in name order, so
	// it opens a file created in the root after .plumb only once it is done
	// with .plumb: a descriptor on that file proves the .plumb registration
	// complete. Each cycle's probe has its own name, because the probe can
	// fall into the same gap, and is removed a cycle later, so it rarely does;
	// a probe that does costs one descriptor, which the slack absorbs.
	var lastProbe string
	removeProbe := func() {
		if lastProbe != "" {
			mustDo(os.Remove(lastProbe))
			lastProbe = ""
		}
	}
	rootRegistered := func(cycle int) {
		t.Helper()
		removeProbe()
		lastProbe = filepath.Join(ws, fmt.Sprintf("probe-%d", cycle))
		mustDo(os.WriteFile(lastProbe, nil, 0o600))
		for deadline := time.Now().Add(10 * time.Second); descriptorsOn(t, lastProbe) < 1; {
			if time.Now().After(deadline) {
				t.Fatalf("cycle %d: the root watcher's reader did not open %s within 10s", cycle, lastProbe)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	// Positive control for descriptorsOn: a bound it cannot exceed proves
	// nothing unless it does see a descriptor that is open.
	probe := filepath.Join(t.TempDir(), "probe")
	mustDo(os.WriteFile(probe, nil, 0o600))
	pf, err := os.Open(probe)
	mustDo(err)
	if n := descriptorsOn(t, probe); n != 1 {
		t.Fatalf("descriptorsOn sees %d descriptors on a file held open once, want 1", n)
	}
	_ = pf.Close()
	before := openFDCount(t)
	for i := range cycles {
		await(func() { mustDo(os.Mkdir(plumbDir, 0o755)) })
		if perWatchDescriptors {
			rootRegistered(i)
		}
		// The attach can trail that dispatch: when the .plumb watcher reports
		// the previous directory's removal only after the new one appeared,
		// the dispatch finds the latch still set, and the re-attach follows
		// that report with a dispatch of its own. So wait for the attach
		// rather than assume it.
		for deadline := time.Now().Add(10 * time.Second); !attached(t, plumbWatcher, plumbDir); {
			if time.Now().After(deadline) {
				t.Fatalf("cycle %d: .plumb not attached 10s after it appeared: .plumb watcher watches %v, %d descriptors on it", i, plumbWatcher.WatchList(), descriptorsOn(t, plumbDir))
			}
			time.Sleep(2 * time.Millisecond)
		}
		if held := descriptorsOn(t, plumbDir); held > 2 {
			t.Fatalf("cycle %d: %d descriptors held for the new .plumb directory, want at most 2 (one per watcher): it was registered twice", i, held)
		}
		await(func() { mustDo(os.Remove(plumbDir)) })
	}
	removeProbe()

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

// TestProjectWatchManager_PlumbDirNeverSharesTheRootWatcher pins the cause the
// descriptor count above can only sample. On kqueue the reader of a watcher
// that watches the root registers every directory created in it, and fsnotify
// does that check-then-open without a lock, so an Add of .plumb on the SAME
// watcher can run inside the reader's own registration. Under load that opens
// .plumb twice, and one descriptor then leaks. The same interleaving also lets
// the reader re-register the directory with its narrower flags after the Add
// (EV_ADD on an existing knote replaces its flags), dropping NOTE_WRITE so
// later config.toml edits go unseen while the watch reports healthy. No
// debounce or delay closes that window; keeping .plumb out of the
// root watcher does, so the test asserts exactly that, whether .plumb exists at
// attach or appears afterwards, and that .plumb is still watched elsewhere.
func TestProjectWatchManager_PlumbDirNeverSharesTheRootWatcher(t *testing.T) {
	for _, tt := range []struct {
		name          string
		presentBefore bool
	}{
		{"present at attach", true},
		{"created after attach", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, sig := testWatchManager(t, nil)
			m.debounce = 10 * time.Millisecond
			built := captureWatchers(m)
			ws := watchedTempDir(t, m)
			root := paths.Canonical(ws)
			plumbDir := filepath.Join(root, ".plumb")
			if tt.presentBefore {
				writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
			}
			m.acquire(ws)
			rootWatcher := nextWatcher(t, built)
			if !slices.Contains(rootWatcher.WatchList(), root) {
				t.Fatalf("first watcher built watches %v, want the workspace root %s", rootWatcher.WatchList(), root)
			}
			if !tt.presentBefore {
				// The loop attaches .plumb before it dispatches, so once the
				// dispatch arrives the attach has happened.
				writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
				awaitDispatch(t, sig, root)
			}
			if slices.Contains(rootWatcher.WatchList(), plumbDir) {
				t.Fatalf(".plumb was added to the watcher that watches the root (%v): that Add races the watcher's own registration of the directory", rootWatcher.WatchList())
			}
			plumbWatcher := nextWatcher(t, built)
			if got := plumbWatcher.WatchList(); !slices.Equal(got, []string{plumbDir}) {
				t.Fatalf("second watcher watches %v, want only %s", got, plumbDir)
			}
			// And it is live: an edit inside .plumb still dispatches. Drain
			// whatever the setup's burst still has in flight first, so the
			// dispatch awaited can only come from this edit.
			for quiet := false; !quiet; {
				select {
				case <-sig:
				case <-time.After(10 * m.debounce):
					quiet = true
				}
			}
			writeProjectCfg(t, ws, "[edits]\nstrict = false\n")
			awaitDispatch(t, sig, root)
		})
	}
}
