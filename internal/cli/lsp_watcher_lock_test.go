//go:build darwin

package cli

// lsp_watcher_lock_test.go — PLAN-488: stopping the LSP file watcher must not
// release this process's fcntl locks on the SQLite files under .plumb. See
// internal/topology/watcher_lock_test.go for the mechanism; the probe helpers
// are config_file_watch_lock_test.go's. Run with CGO_ENABLED=0 as well as with
// cgo: release builds are CGO_ENABLED=0, and that is where the kqueue backend
// this replaced opened every file in the tree.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLSPFSWatcher_StopKeepsSQLiteLocksInPlumbDir is the PLAN-488 regression
// for the LSP watcher, which every pool hibernate and close stops.
func TestLSPFSWatcher_StopKeepsSQLiteLocksInPlumbDir(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := writeSQLiteSiblings(t, plumbDir, "collab.db")
	db := files[0]
	holdFcntlLock(t, db)
	// Positive control: the probe sees a lock that is held.
	if !lockedByUs(t, db) {
		t.Fatal("the probe does not see a lock this process holds")
	}
	base := make([]int, len(files))
	for i, f := range files {
		base[i] = descriptorsOn(t, f)
	}

	rec := &watchedFilesRecordingClient{stubClient: &stubClient{}}
	proxy := &clientProxy{}
	proxy.set(rec)
	fw, err := newLSPFSWatcher(ws, proxy)
	if err != nil {
		t.Fatalf("newLSPFSWatcher: %v", err)
	}
	fw.Start()
	t.Cleanup(fw.Stop)
	// Wait for a delivered event, so the watcher is fully established (a
	// tree-walking backend has opened every descriptor by then). Each rewrite
	// waits out the watcher's cooldown, which a faster cadence would keep
	// re-arming. This also needs the exclusion anchored at the workspace: under
	// `make test` the workspace lives below .testcache, a dot-prefixed ancestor
	// the old unanchored pattern matched, so no event ever arrived.
	deadline := time.Now().Add(30 * time.Second)
	for rec.totalEvents() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the watcher delivered no event, so the test cannot tell it is established")
		}
		if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2*lspWatchCooldown + 100*time.Millisecond)
	}
	for i, f := range files {
		if n := descriptorsOn(t, f) - base[i]; n != 0 {
			t.Errorf("the watcher holds %d descriptor(s) on .plumb/%s; closing them releases the daemon's SQLite locks", n, filepath.Base(f))
		}
	}

	fw.Stop()
	if !lockedByUs(t, db) {
		t.Fatal("stopping the LSP watcher released this process's fcntl lock on .plumb/collab.db: it held a descriptor on a database file")
	}
}
