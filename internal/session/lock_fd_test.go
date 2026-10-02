package session_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
)

// TestSessionDirLock_OneDescriptorWaits guards #583: while another holder has
// .sessions.lock, queued registry writes in this process must wait in memory,
// not each on its own open descriptor blocked in flock. Before the fix the
// daemon held one descriptor (and goroutine) per pending write — ~500 on the
// live registry — which is unbounded and heads for the fd limit.
//
// The holder is a separate open file description in this process; flock
// conflicts between descriptions, not processes, so it blocks the writers
// exactly as a foreign process would.
func TestSessionDirLock_OneDescriptorWaits(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir, err := session.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	holder, err := os.OpenFile(filepath.Join(dir, ".sessions.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock: %v", err)
	}
	var lockStat syscall.Stat_t
	if err := syscall.Fstat(int(holder.Fd()), &lockStat); err != nil {
		t.Fatalf("fstat: %v", err)
	}

	const writers = 32
	errs := make(chan error, writers)
	for i := range writers {
		go func() {
			_, err := session.Register(session.Info{ID: "w" + strconv.Itoa(i), Folder: "/tmp/x"})
			errs <- err
		}()
	}

	// Positive control: wait until a writer is actually blocked on the lock
	// file, so a count of zero waiters cannot pass for "bounded".
	deadline := time.Now().Add(5 * time.Second)
	for openLockFDs(t, lockStat)-1 < 1 {
		if time.Now().After(deadline) {
			t.Fatal("no writer opened the lock file while it was held")
		}
		time.Sleep(time.Millisecond)
	}
	// Give every writer time to reach the lock, sampling the peak.
	maxWaiting := 0
	for end := time.Now().Add(200 * time.Millisecond); time.Now().Before(end); {
		maxWaiting = max(maxWaiting, openLockFDs(t, lockStat)-1)
		time.Sleep(time.Millisecond)
	}
	if maxWaiting > 1 {
		t.Errorf("%d descriptors waited on .sessions.lock at once, want at most 1", maxWaiting)
	}
	if n := len(errs); n != 0 {
		t.Fatalf("%d writers finished while the lock was held", n)
	}

	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	for range writers {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("Register: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("writers did not all finish after the lock was released")
		}
	}
	infos, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != writers {
		t.Errorf("List returned %d sessions, want %d", len(infos), writers)
	}
	// Back to baseline: only the holder still has the lock file open. Every
	// writer's descriptor was closed, none leaked behind the queue.
	if n := openLockFDs(t, lockStat); n != 1 {
		t.Errorf("%d descriptors open on .sessions.lock after every writer finished, want 1 (the holder)", n)
	}
}

// TestSessionDirLock_ReleasedOnEveryPath guards the other half of #583's
// bound: the in-process mutex a writer queues on must be released, and the lock
// descriptor closed, on every way out of withSessionDirLock. A path that kept
// the mutex would park every later registry write in the process forever, with
// no descriptor to show for it, which is worse than the leak #583 fixed.
func TestSessionDirLock_ReleasedOnEveryPath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	base := t.TempDir()

	// acquires reports the result of a fresh acquisition of dir's lock, failing
	// the test if it does not return: a held mutex hangs here, it does not error.
	acquires := func(t *testing.T, dir string) error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- session.WithSessionDirLockForTest(dir, func() error { return nil }) }()
		select {
		case err := <-done:
			return err
		case <-time.After(3 * time.Second):
			t.Fatalf("withSessionDirLock(%s) did not return: the in-process mutex was not released", dir)
			return nil
		}
	}

	t.Run("panic in fn", func(t *testing.T) {
		dir := filepath.Join(base, "panic")
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic did not propagate")
				}
			}()
			_ = session.WithSessionDirLockForTest(dir, func() error { panic("boom") })
		}()
		if err := acquires(t, dir); err != nil {
			t.Errorf("acquire after a panic: %v", err)
		}
		if n := openLockFDsAt(t, dir); n != 0 {
			t.Errorf("%d descriptors left open on .sessions.lock after a panic", n)
		}
	})

	t.Run("error from fn", func(t *testing.T) {
		dir := filepath.Join(base, "fnerr")
		want := os.ErrInvalid
		if err := session.WithSessionDirLockForTest(dir, func() error { return want }); !errors.Is(err, want) {
			t.Fatalf("got %v, want fn's error back", err)
		}
		if err := acquires(t, dir); err != nil {
			t.Errorf("acquire after an fn error: %v", err)
		}
		if n := openLockFDsAt(t, dir); n != 0 {
			t.Errorf("%d descriptors left open on .sessions.lock after an fn error", n)
		}
	})

	t.Run("directory cannot be created", func(t *testing.T) {
		file := filepath.Join(base, "afile")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(file, "sub") // a path under a regular file
		if err := session.WithSessionDirLockForTest(dir, func() error { return nil }); err == nil {
			t.Fatal("want an error creating a directory under a regular file")
		}
		if err := acquires(t, dir); err == nil {
			t.Error("want the same error again, not a success")
		}
	})

	t.Run("lock file cannot be opened", func(t *testing.T) {
		dir := filepath.Join(base, "noopen")
		// A directory where the lock file belongs: MkdirAll succeeds, OpenFile
		// fails, after the mutex has been taken.
		if err := os.MkdirAll(filepath.Join(dir, ".sessions.lock"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := session.WithSessionDirLockForTest(dir, func() error { return nil }); err == nil {
			t.Fatal("want an error opening a lock path that is a directory")
		}
		if err := acquires(t, dir); err == nil {
			t.Error("want the same error again, not a success")
		}
	})
}

// openLockFDs counts this process's descriptors open on the file lockStat
// describes, matching by device and inode through the fd table.
func openLockFDs(t *testing.T, lockStat syscall.Stat_t) int {
	t.Helper()
	// Names only: os.ReadDir stats each /dev/fd entry on macOS, and on some
	// macOS releases (CI's runner, not every local one) an entry fails that
	// stat with EBADF, failing the whole read on Go releases without the
	// go.dev/issue/80143 workaround. The Fstat below already skips descriptors
	// it cannot stat.
	fdDir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatalf("open fd table: %v", err)
	}
	names, err := fdDir.Readdirnames(-1)
	fdDir.Close()
	if err != nil {
		t.Fatalf("read fd table: %v", err)
	}
	n := 0
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		// A descriptor closed since the listing fails Fstat; skip it.
		if syscall.Fstat(fd, &st) == nil && st.Dev == lockStat.Dev && st.Ino == lockStat.Ino {
			n++
		}
	}
	return n
}

// openLockFDsAt counts this process's descriptors open on dir's .sessions.lock.
func openLockFDsAt(t *testing.T, dir string) int {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(dir, ".sessions.lock"), &st); err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	return openLockFDs(t, st)
}
