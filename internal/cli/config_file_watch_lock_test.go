//go:build !windows

package cli

// config_file_watch_lock_test.go — PLAN-485: a config watcher must never hold
// a descriptor on, and so never drop this process's locks on, the SQLite
// files that share its directory.
//
// On kqueue a watch on a DIRECTORY opens every file in it, and closing any
// descriptor to a file releases every fcntl lock the process holds on it. The
// config watchers used to watch <workspace>/.plumb and the global config
// directory, which hold the daemon's databases; each closed watch silently
// stripped SQLite's locks, and another process then unlinked the WAL the
// daemon was still writing. On Linux inotify holds no per-file descriptor, so
// these tests hold trivially there; they bite on macOS and the BSDs.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
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
		t.Skip("helper process for the PLAN-485 lock tests")
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

// writeSQLiteSiblings creates empty files named like a database and its WAL
// sidecars in dir, and returns their paths.
func writeSQLiteSiblings(t *testing.T, dir, name string) []string {
	t.Helper()
	out := make([]string, 0, 3)
	for _, p := range []string{name, name + "-wal", name + "-shm"} {
		path := filepath.Join(dir, p)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		out = append(out, path)
	}
	return out
}

// TestProjectWatchManager_KeepsSQLiteLocksInPlumbDir is the PLAN-485
// regression, end to end: a lock this process holds on a database in .plumb
// must survive a watch being attached, a sidecar churning, and the watcher
// being closed. Before the fix the close alone released it.
func TestProjectWatchManager_KeepsSQLiteLocksInPlumbDir(t *testing.T) {
	m, sig := testWatchManager(t, nil)
	ws := watchedTempDir(t, m)
	writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	files := writeSQLiteSiblings(t, filepath.Join(ws, ".plumb"), "collab.db")
	db, wal := files[0], files[1]
	holdFcntlLock(t, db)
	// Positive control: the probe sees a lock that is held. Without it, a
	// probe that always said "unlocked" would make the assertions below
	// impossible to fail for the wrong reason, and one that always said
	// "locked" would make them pass for the wrong reason.
	if !lockedByUs(t, db) {
		t.Fatal("the probe does not see a lock this process holds")
	}

	m.acquire(ws)
	if err := os.Remove(wal); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A config edit proves the watch is live and has handled the churn above.
	writeProjectCfg(t, ws, "[edits]\nstrict = false\n")
	awaitDispatch(t, sig, paths.Canonical(ws))
	m.close() // waits for the watch goroutine to close its OS watchers

	if !lockedByUs(t, db) {
		t.Fatal("the project config watcher released this process's fcntl lock on .plumb/collab.db: it held a descriptor on a file it does not watch")
	}
}

// TestProjectWatchManager_HoldsNoDescriptorOnPlumbSiblings pins the cause the
// lock test samples: the watcher holds a descriptor on config.toml and on
// nothing else in .plumb. Only kqueue holds per-watch descriptors.
func TestProjectWatchManager_HoldsNoDescriptorOnPlumbSiblings(t *testing.T) {
	if !perWatchDescriptors {
		t.Skip("only kqueue holds a descriptor per watched file")
	}
	m, _ := testWatchManager(t, nil)
	ws := watchedTempDir(t, m)
	writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	plumbDir := filepath.Join(ws, ".plumb")
	siblings := writeSQLiteSiblings(t, plumbDir, "topology.db")
	m.acquire(ws)

	// Positive control: the watch is attached and descriptorsOn can see it.
	if n := descriptorsOn(t, filepath.Join(plumbDir, "config.toml")); n < 1 {
		t.Fatalf("no descriptor on config.toml after acquire (%d): the file is not watched, so the absence checks below prove nothing", n)
	}
	for _, s := range siblings {
		if n := descriptorsOn(t, s); n != 0 {
			t.Errorf("%d descriptor(s) held on %s: the watcher opened a file in .plumb other than config.toml", n, filepath.Base(s))
		}
	}
}

// TestGlobalConfigWatcher_HoldsNoDescriptorOnDataFiles is the same check for
// the global watcher, whose directory on a fresh macOS install is the data
// directory holding stats.db and the other global databases.
func TestGlobalConfigWatcher_HoldsNoDescriptorOnDataFiles(t *testing.T) {
	if !perWatchDescriptors {
		t.Skip("only kqueue holds a descriptor per watched file")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(func(c *config.Config) { c.Edits.Strict = true }); err != nil {
		t.Fatalf("Save: %v", err)
	}
	gw := newGlobalConfigWatcher(config.NewStore(config.Defaults()))
	siblings := writeSQLiteSiblings(t, gw.dir, "stats.db")
	holdFcntlLock(t, siblings[0])

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx) }()

	cfgPath := filepath.Join(gw.dir, gw.base)
	for deadline := time.Now().Add(10 * time.Second); descriptorsOn(t, cfgPath) < 1; {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the global watcher never attached config.toml, so the absence checks below prove nothing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i, s := range siblings {
		// stats.db itself is held open once, by holdFcntlLock.
		want := 0
		if i == 0 {
			want = 1
		}
		if n := descriptorsOn(t, s); n != want {
			t.Errorf("%d descriptor(s) held on %s, want %d: the global watcher opened a data file", n, filepath.Base(s), want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !lockedByUs(t, siblings[0]) {
		t.Fatal("the global config watcher released this process's fcntl lock on stats.db")
	}
}
