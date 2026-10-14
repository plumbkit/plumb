package cli

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"testing"
)

// `plumb stop`'s own file hygiene (PLAN-551): the pid file is removed only while
// it still names the stale pid, and the sweep claims every legacy location.

// deadPID returns a pid that certainly belonged to a process that has exited
// and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running the stand-in: %v", err)
	}
	return cmd.Process.Pid
}

func TestRemoveStalePIDFile_RemovesOnlyTheStaleRecord(t *testing.T) {
	withShortTempRuntime(t)
	stale := deadPID(t)

	writeDaemonPIDFile(t, stale)
	path := daemonPIDPath()
	removeStalePIDFile(path, stale)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the pid file still names the stale pid %d and must be removed (stat err %v)", stale, err)
	}

	// A new daemon published its own pid file between stop's read and the unlink:
	// that file is not stop's to remove.
	writeDaemonPIDFile(t, os.Getpid())
	removeStalePIDFile(path, stale)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stop removed a pid file that names another process (%d): %v", os.Getpid(), err)
	}
}

// TestStopByPID_LeavesAnotherDaemonsPIDFile drives the path `plumb stop` takes for
// a pid that is already gone: it used to unlink the pid file whatever it named.
func TestStopByPID_LeavesAnotherDaemonsPIDFile(t *testing.T) {
	withShortTempRuntime(t)
	writeDaemonPIDFile(t, os.Getpid()) // somebody else's live record
	path := daemonPIDPath()
	if err := stopByPID(deadPID(t), false); err != nil {
		t.Fatalf("stopByPID: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stopping a dead pid removed the pid file of a live process: %v", err)
	}
}

// TestOwnedRuntimeDirs_ClaimsEveryLegacyLocation: in canonical mode the
// environment-derived runtime directory is a legacy location too, and a daemon an
// older binary left there is one `plumb stop` must be able to find.
func TestOwnedRuntimeDirs_ClaimsEveryLegacyLocation(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	if runtime.GOOS == "linux" {
		// A real $XDG_RUNTIME_DIR, so the environment-derived location differs
		// from the cache one (on darwin the environment's runtime directory IS the
		// cache directory, and the two are one location).
		xdg, err := os.MkdirTemp("/tmp", "plumb-xdg-") //nolint:usetesting // a short path keeps sockets under sun_path
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(xdg) })
		if err := os.Chmod(xdg, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_RUNTIME_DIR", xdg)
	}
	locs := legacyLocations()
	if len(locs) == 0 {
		t.Fatal("this isolated account names no legacy location: the test proves nothing")
	}
	dirs := ownedRuntimeDirs()
	for _, loc := range locs {
		if !slices.Contains(dirs, loc.Dir) {
			t.Errorf("the legacy location %s is not claimed by plumb stop's sweep: %v", loc.Dir, dirs)
		}
	}
}

func TestOwnedDirsFrom_DedupsEveryLegacyLocation(t *testing.T) {
	got := ownedDirsFrom("/run/a", "/cache/b", "", "/run/a", "/env/c", "/cache/b")
	if want := []string{"/run/a", "/cache/b", "/env/c"}; !slices.Equal(got, want) {
		t.Fatalf("ownedDirsFrom = %v, want %v", got, want)
	}
}

// TestRemoveStalePIDFile_KeepsAFileReplacedAfterTheMark pins the order: the file
// is marked BEFORE it is read, and removed only through that identity. A file
// replaced in between — even one naming the same stale pid — is a different file
// and is left alone. Reading first, or removing by path, would delete it.
func TestRemoveStalePIDFile_KeepsAFileReplacedAfterTheMark(t *testing.T) {
	withShortTempRuntime(t)
	stale := deadPID(t)
	writeDaemonPIDFile(t, stale)
	path := daemonPIDPath()
	old := afterPIDFileMarked
	t.Cleanup(func() { afterPIDFileMarked = old })
	afterPIDFileMarked = func() {
		_ = os.Remove(path)
		writeDaemonPIDFile(t, stale) // a new file (new inode) with the same contents
	}
	removeStalePIDFile(path, stale)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a pid file replaced after the mark was removed: %v", err)
	}
}
