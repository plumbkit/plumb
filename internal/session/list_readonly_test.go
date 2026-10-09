package session_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
)

// writeRegistryFile writes one raw session file the way a serve process would.
func writeRegistryFile(t *testing.T, dir, id string, pid int) string {
	t.Helper()
	path := filepath.Join(dir, id+".json")
	body := `{"id":"` + id + `","name":"` + id + `-name","pid":` + strconv.Itoa(pid) +
		`,"language":"go","folder":"/tmp/ws","adapter":"gopls","started_at":"` + time.Now().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestListForReading_FallsBackWhenTheLockCannotBeOpened is PLAN-495's sandbox
// case: the lock file exists but this process may not open it for writing, the
// way a harness that denies writes under the data dir refuses it. The registry is
// still read — and read WITHOUT a single write, so the dead session's file is not
// marked ended the way List would mark it.
func TestListForReading_FallsBackWhenTheLockCannotBeOpened(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode-0000 file regardless, so the refused open cannot be simulated")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir, err := session.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRegistryFile(t, dir, "live", os.Getpid())
	dead := writeRegistryFile(t, dir, "dead", 999999999)
	lock := filepath.Join(dir, ".sessions.lock")
	if err := os.WriteFile(lock, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lock, 0o644) })

	infos, locked, err := session.ListForReading()
	if err != nil {
		t.Fatalf("ListForReading with an unopenable lock = %v, want the read-only fallback", err)
	}
	if locked {
		t.Error("locked = true, but the lock file could not have been opened")
	}
	if len(infos) != 1 || infos[0].ID != "live" {
		t.Fatalf("infos = %+v, want exactly the live session", infos)
	}
	data, err := os.ReadFile(dead)
	if err != nil {
		t.Fatalf("the dead session's file was removed by a read-only listing: %v", err)
	}
	if strings.Contains(string(data), "ended_at") {
		t.Error("the read-only fallback wrote ended_at into the dead session's file; it must not write at all")
	}
}

// TestListForReading_UsesTheLockWhenItCan is the control: with the lock available
// the answer comes from List itself, side effects included — the dead session is
// marked ended on disk, which only the locked path does.
func TestListForReading_UsesTheLockWhenItCan(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir, err := session.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRegistryFile(t, dir, "live", os.Getpid())
	dead := writeRegistryFile(t, dir, "dead", 999999999)

	infos, locked, err := session.ListForReading()
	if err != nil {
		t.Fatalf("ListForReading: %v", err)
	}
	if !locked {
		t.Error("locked = false with an openable lock; the fallback must be the exception, not the path")
	}
	if len(infos) != 1 || infos[0].ID != "live" {
		t.Fatalf("infos = %+v, want exactly the live session", infos)
	}
	data, err := os.ReadFile(dead)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ended_at") {
		t.Error("the locked path did not mark the dead session ended; it should behave exactly like List")
	}
}
