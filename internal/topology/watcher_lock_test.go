//go:build darwin

package topology

// watcher_lock_test.go — PLAN-488: stopping the workspace watcher must not
// release this process's fcntl locks on the SQLite files under .plumb.
//
// On macOS, closing ANY descriptor to a file releases every fcntl lock the
// process holds on it. Release builds (CGO_ENABLED=0) used to watch through
// sgtdi/fswatcher's kqueue backend, which opened a descriptor on every file in
// the tree, .plumb's databases included, and closed them all on stop; the next
// process to open a database then believed it was alone, checkpointed, and
// unlinked the WAL the daemon was still writing. The watcher is now
// internal/fswatch, whose darwin backend is FSEvents in every build. This test
// pins the property through the topology watcher itself, and must be run with
// CGO_ENABLED=0 as well as with cgo: that is the build users get.
// internal/fswatch's own lock tests cover a real WAL database, symlinks and
// repeated cycles.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lockProbeEnv names the file a lock-probe child process inspects.
const lockProbeEnv = "PLUMB_TEST_FCNTL_PROBE"

// TestHelperFcntlLockProbe is not a test: run as a child process with
// lockProbeEnv set, it reports whether ANOTHER process holds an fcntl lock on
// that file. F_GETLK never reports the caller's own locks, which is why the
// question needs a second process.
func TestHelperFcntlLockProbe(t *testing.T) {
	path := os.Getenv(lockProbeEnv)
	if path == "" {
		t.Skip("helper process for the PLAN-488 lock tests")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fmt.Println("probe-error:", err)
		return
	}
	defer f.Close()
	lk := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
		fmt.Println("probe-error:", err)
		return
	}
	if lk.Type == syscall.F_UNLCK {
		fmt.Println("probe:unlocked")
	} else {
		fmt.Println("probe:locked")
	}
}

// lockedByUs asks a child process whether this process still holds its fcntl
// lock on path.
func lockedByUs(t *testing.T, path string) bool {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperFcntlLockProbe$", "-test.count=1")
	cmd.Env = append(os.Environ(), lockProbeEnv+"="+path)
	out, err := cmd.CombinedOutput()
	switch {
	case strings.Contains(string(out), "probe:locked"):
		return true
	case strings.Contains(string(out), "probe:unlocked"):
		return false
	default:
		t.Fatalf("lock probe gave no answer (err %v): %s", err, out)
		return false
	}
}

// holdFcntlLock takes a whole-file write lock on path, as SQLite does on its
// database and shared-memory files, and keeps it until the test ends.
func holdFcntlLock(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	lk := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		t.Fatal(err)
	}
}

// descriptorsOn reports how many of this process's descriptors refer to the
// file at path, by fstat-ing each descriptor /dev/fd lists and comparing device
// and inode. (Statting the /dev/fd entries by name describes the entry, not the
// open file, on macOS.) An entry that cannot be statted is the listing's own
// descriptor or one closed meanwhile, and neither is path.
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

// TestFSWatcher_StopKeepsSQLiteLocksInPlumbDir is the PLAN-488 regression: a
// lock this process holds on .plumb/collab.db must survive the topology
// watcher starting and stopping.
func TestFSWatcher_StopKeepsSQLiteLocksInPlumbDir(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{"collab.db", "collab.db-wal", "collab.db-shm", "topology.db"}
	files := make([]string, 0, len(names))
	for _, name := range names {
		p := filepath.Join(plumbDir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	db := files[0]
	holdFcntlLock(t, db)
	// Positive control: the probe sees a lock that is held. Without it a probe
	// that always answered "locked" would let the final assertion pass for the
	// wrong reason.
	if !lockedByUs(t, db) {
		t.Fatal("the probe does not see a lock this process holds")
	}
	base := make([]int, len(files))
	for i, f := range files {
		base[i] = descriptorsOn(t, f)
	}

	sink := &recordingSink{}
	fw, err := newFSWatcher(ws, sink, nil)
	if err != nil {
		t.Fatalf("newFSWatcher: %v", err)
	}
	fw.Start()
	t.Cleanup(fw.Stop)
	// Wait until the watcher delivers an event, so it is fully established: a
	// backend that walks the tree (kqueue) has opened every descriptor it will
	// open by the time events flow.
	warm := waitFor(func() bool {
		if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return waitFor(func() bool {
			enq, _ := sink.snapshot()
			return slices.Contains(enq, "main.go")
		}, 500*time.Millisecond)
	}, 30*time.Second)
	if !warm {
		t.Fatal("the watcher delivered no event, so the test cannot tell it is established")
	}
	for i, f := range files {
		if n := descriptorsOn(t, f) - base[i]; n != 0 {
			t.Errorf("the watcher holds %d descriptor(s) on .plumb/%s; closing them releases the daemon's SQLite locks", n, filepath.Base(f))
		}
	}

	fw.Stop()
	if !lockedByUs(t, db) {
		t.Fatal("stopping the topology watcher released this process's fcntl lock on .plumb/collab.db: it held a descriptor on a database file")
	}
}
