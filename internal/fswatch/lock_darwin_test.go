//go:build darwin

package fswatch

// lock_darwin_test.go — PLAN-488. On macOS, closing ANY descriptor to a file
// releases every fcntl lock the process holds on it. A watcher that opens the
// files it watches therefore strips SQLite's locks the moment it closes them,
// and the next process to open the database believes it is alone, checkpoints,
// and unlinks the WAL this process is still writing. These tests hold a real
// SQLite database open in WAL mode under .plumb, as the daemon does, and check
// from a child process that its locks survive the watcher. F_GETLK never
// reports the caller's own locks, which is why the question needs a second
// process. Run them with CGO_ENABLED=0 as well as with cgo: release builds are
// CGO_ENABLED=0, and that is the configuration that used to fail.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

// lockProbeEnv names the file a probe child process inspects.
const lockProbeEnv = "PLUMB_FSWATCH_LOCK_PROBE"

// TestHelperLockProbe is not a test: run as a child process with lockProbeEnv
// set, it reports whether another process holds any fcntl lock on that file.
func TestHelperLockProbe(t *testing.T) {
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
	lk := syscall.Flock_t{Type: syscall.F_WRLCK} // whole file: start 0, len 0
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

func lockedByUs(t *testing.T, path string) bool {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLockProbe$", "-test.count=1")
	cmd.Env = append(os.Environ(), lockProbeEnv+"="+path)
	out, err := cmd.CombinedOutput()
	switch {
	case strings.Contains(string(out), "probe:locked"):
		return true
	case strings.Contains(string(out), "probe:unlocked"):
		return false
	}
	t.Fatalf("lock probe gave no answer (err %v): %s", err, out)
	return false
}

// descriptorsOn counts this process's descriptors on the file at path by
// fstat-ing each /dev/fd entry and comparing device and inode (statting the
// entries by name describes the entry, not the open file, on macOS).
func descriptorsOn(t *testing.T, path string) int {
	t.Helper()
	var want syscall.Stat_t
	if err := syscall.Stat(path, &want); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil {
		t.Fatal(err)
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

// liveDatabase opens a WAL database under root/.plumb through sqlitex, as the
// daemon does, and holds a read transaction so SQLite keeps its locks on both
// the database and its shared-memory file for the whole test. It returns the
// three file paths: database, -wal, -shm.
func liveDatabase(t *testing.T, root string) (*sql.DB, [3]string) {
	t.Helper()
	path := filepath.Join(root, ".plumb", "collab.db")
	db, err := sqlitex.Open(path, sqlitex.Options{MaxOpenConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		if _, err := db.Exec(`INSERT INTO notes (body) VALUES (?)`, fmt.Sprint("note ", i)); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM notes`).Scan(&n); err != nil || n != 50 {
		t.Fatalf("read inside the held transaction: n=%d err=%v", n, err)
	}
	files := [3]string{path, path + "-wal", path + "-shm"}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s missing; the database is not in WAL mode: %v", filepath.Base(f), err)
		}
	}
	// Positive control: the probe sees SQLite's locks. Without it a probe that
	// always answered "locked" would let every assertion below pass for the
	// wrong reason.
	for _, f := range []string{files[0], files[2]} {
		if !lockedByUs(t, f) {
			t.Fatalf("the probe does not see SQLite's lock on %s", filepath.Base(f))
		}
	}
	return db, files
}

// TestWatcher_KeepsSQLiteLocks is the PLAN-488 regression. It covers a source
// file symlinked to the live database and to its shared-memory file (a
// backend that opened entries would open the database through them), and
// repeated start/stop cycles, the way pool hibernation churns watchers.
func TestWatcher_KeepsSQLiteLocks(t *testing.T) {
	root := t.TempDir()
	db, files := liveDatabase(t, root)
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%d.go", i)), []byte("package src\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(files[0], filepath.Join(src, "linked.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(files[2], filepath.Join(root, "linked.db-shm")); err != nil {
		t.Fatal(err)
	}
	var base [3]int
	for i, f := range files {
		base[i] = descriptorsOn(t, f)
	}

	for cycle := range 10 {
		w, err := New(root, Options{Cooldown: 20 * time.Millisecond, ExcludeRegex: ExcludeDirsRegex(root)})
		if err != nil {
			t.Fatalf("cycle %d: New: %v", cycle, err)
		}
		c := &collector{}
		go func() {
			for ev := range w.Events() {
				c.mu.Lock()
				c.evs = append(c.evs, ev)
				c.mu.Unlock()
			}
		}()
		go func() {
			for range w.Lost() {
				c.mu.Lock()
				c.lost++
				c.mu.Unlock()
			}
		}()
		marker := filepath.Join(src, fmt.Sprintf("cycle%d.go", cycle))
		ok := waitFor(func() bool {
			if err := os.WriteFile(marker, []byte("package src\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// Touch the symlinks too, so any backend that resolves them sees them.
			now := time.Now()
			_ = os.Chtimes(filepath.Join(src, "linked.db"), now, now)
			return waitFor(func() bool { return c.count(marker, 0) > 0 }, 500*time.Millisecond)
		}, 30*time.Second)
		if !ok {
			w.Close()
			t.Fatalf("cycle %d: the watcher delivered no event, so it cannot be shown established", cycle)
		}
		for i, f := range files {
			if n := descriptorsOn(t, f) - base[i]; n != 0 {
				t.Errorf("cycle %d: the watcher holds %d descriptor(s) on %s", cycle, n, filepath.Base(f))
			}
		}
		w.Close()
		for _, f := range []string{files[0], files[2]} {
			if !lockedByUs(t, f) {
				t.Fatalf("cycle %d: closing the watcher released this process's SQLite lock on %s", cycle, filepath.Base(f))
			}
		}
	}

	// The database is still whole and writable through the same handle.
	if _, err := db.Exec(`INSERT INTO notes (body) VALUES ('after')`); err != nil {
		t.Fatalf("write after the cycles: %v", err)
	}
}
