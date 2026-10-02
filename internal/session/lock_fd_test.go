package session_test

import (
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
