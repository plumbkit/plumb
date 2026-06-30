package txlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// initWorkspace creates a temp dir with a .plumb/ subdirectory to satisfy
// Begin's marker check, returning the workspace root.
func initWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".plumb"), 0o755); err != nil {
		t.Fatalf("creating .plumb: %v", err)
	}
	return dir
}

// TestBegin_NoPlumb returns a no-op Log when the workspace has no .plumb/.
func TestBegin_NoPlumb(t *testing.T) {
	ws := t.TempDir() // no .plumb/
	l, err := Begin(ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.dir != "" {
		t.Errorf("expected no-op Log (empty dir), got dir=%q", l.dir)
	}
}

// TestBegin_EmptyWorkspace returns a no-op Log without error.
func TestBegin_EmptyWorkspace(t *testing.T) {
	l, err := Begin("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.dir != "" {
		t.Errorf("expected no-op Log, got dir=%q", l.dir)
	}
}

// TestHappyPath verifies that a successful transaction leaves no tx-log dir.
func TestHappyPath(t *testing.T) {
	ws := initWorkspace(t)
	target := filepath.Join(ws, "file.txt")
	if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := Begin(ws)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	txDir := l.dir
	if txDir == "" {
		t.Fatal("expected a real tx-log dir, got no-op Log")
	}

	if err := l.Record(target, []byte("before"), 0o644); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Simulate a successful write.
	if err := os.WriteFile(target, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.Commit()

	if _, err := os.Stat(txDir); !os.IsNotExist(err) {
		t.Error("tx-log dir should be gone after Commit")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "after" {
		t.Errorf("target content = %q, want %q", got, "after")
	}
}

// TestRollback verifies that Rollback restores files and removes the tx-log dir.
func TestRollback(t *testing.T) {
	ws := initWorkspace(t)
	target := filepath.Join(ws, "file.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := Begin(ws)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	txDir := l.dir

	if err := l.Record(target, []byte("original"), 0o644); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Simulate a partial write (write succeeds, then the transaction fails).
	if err := os.WriteFile(target, []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}

	l.Rollback()

	// tx-log dir must be gone.
	if _, err := os.Stat(txDir); !os.IsNotExist(err) {
		t.Error("tx-log dir should be gone after Rollback")
	}
	// File must be restored.
	got, _ := os.ReadFile(target)
	if string(got) != "original" {
		t.Errorf("content after rollback = %q, want %q", got, "original")
	}
}

// TestScan_CrashSimulation writes a tx-log directory manually (simulating a
// daemon crash mid-transaction) and confirms Scan restores the files.
func TestScan_CrashSimulation(t *testing.T) {
	ws := initWorkspace(t)
	target := filepath.Join(ws, "file.txt")
	if err := os.WriteFile(target, []byte("modified by crash"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Manually build an orphaned tx-log directory.
	txID := "crash-sim-1"
	txDir := filepath.Join(ws, ".plumb", "tx-log", txID)
	if err := os.MkdirAll(txDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := txManifest{
		TxID:      txID,
		Workspace: ws,
		Ops: []opMeta{
			{N: 0, Path: target, Perm: 0o644, Snapshotted: true},
		},
	}
	manifestData, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(txDir, "manifest.json"), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	snapPath := filepath.Join(txDir, "0-before")
	if err := os.WriteFile(snapPath, []byte("pre-crash"), 0o600); err != nil {
		t.Fatal(err)
	}

	Scan(ws, time.Now().Add(time.Hour))

	// tx-log dir must be gone.
	if _, err := os.Stat(txDir); !os.IsNotExist(err) {
		t.Error("orphaned tx-log dir should be removed after Scan")
	}
	// File must be restored to pre-crash snapshot.
	got, _ := os.ReadFile(target)
	if string(got) != "pre-crash" {
		t.Errorf("restored content = %q, want %q", got, "pre-crash")
	}
}

// TestScan_NopWhenEmpty is a no-op when no tx-log directory exists.
func TestScan_NopWhenEmpty(t *testing.T) {
	ws := initWorkspace(t)
	Scan(ws, time.Now().Add(time.Hour)) // must not panic or error
}

// TestScan_EmptyWorkspace is a no-op on empty workspace.
func TestScan_EmptyWorkspace(t *testing.T) {
	Scan("", time.Now()) // must not panic
}

// TestConcurrentTransactions verifies that concurrent transactions on disjoint
// paths each get their own txID directory with no interference.
func TestConcurrentTransactions(t *testing.T) {
	ws := initWorkspace(t)
	a := filepath.Join(ws, "a.txt")
	b := filepath.Join(ws, "b.txt")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	la, err := Begin(ws)
	if err != nil {
		t.Fatalf("Begin(a): %v", err)
	}
	lb, err := Begin(ws)
	if err != nil {
		t.Fatalf("Begin(b): %v", err)
	}
	if la.dir == lb.dir {
		t.Fatal("concurrent transactions must have distinct tx-log dirs")
	}

	if err := la.Record(a, []byte("before"), 0o644); err != nil {
		t.Fatalf("la.Record: %v", err)
	}
	if err := lb.Record(b, []byte("before"), 0o644); err != nil {
		t.Fatalf("lb.Record: %v", err)
	}
	la.Commit()
	lb.Rollback()

	// a was committed — its dir is gone, its content unchanged.
	if _, err := os.Stat(la.dir); !os.IsNotExist(err) {
		t.Error("la tx-log should be gone after Commit")
	}
	// b was rolled back — its dir is gone, its content restored.
	if _, err := os.Stat(lb.dir); !os.IsNotExist(err) {
		t.Error("lb tx-log should be gone after Rollback")
	}
}

// TestRecord_LargeFileSkipsSnapshot checks that files over maxSnapSize are
// recorded in the manifest but not snapshotted.
func TestRecord_LargeFileSkipsSnapshot(t *testing.T) {
	ws := initWorkspace(t)

	l, err := Begin(ws)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer l.Commit()

	large := make([]byte, maxSnapSize+1)
	if err := l.Record("/tmp/huge.go", large, 0o644); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Snapshot file must NOT exist.
	snapPath := filepath.Join(l.dir, strconv.Itoa(0)+"-before")
	if _, err := os.Stat(snapPath); !os.IsNotExist(err) {
		t.Error("snapshot file should not exist for oversized content")
	}
	// Manifest must record the op with snapshotted=false.
	data, _ := os.ReadFile(filepath.Join(l.dir, "manifest.json"))
	var m txManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(m.Ops) != 1 {
		t.Fatalf("expected 1 op in manifest, got %d", len(m.Ops))
	}
	if m.Ops[0].Snapshotted {
		t.Error("large-file op should have snapshotted=false")
	}
}

// TestNoOpLog verifies that all methods on a no-op Log are safe to call.
func TestNoOpLog(t *testing.T) {
	l := &Log{} // zero value = no-op
	if err := l.Record("/any/path", []byte("content"), 0o644); err != nil {
		t.Errorf("no-op Record: %v", err)
	}
	l.Commit()   // must not panic
	l.Rollback() // must not panic
}

// TestRollback_MultiFile verifies that Rollback restores all recorded files
// when a transaction writes A then fails before writing B. This is the
// primary correctness scenario the txlog exists for.
func TestRollback_MultiFile(t *testing.T) {
	ws := initWorkspace(t)
	a := filepath.Join(ws, "a.txt")
	b := filepath.Join(ws, "b.txt")
	for _, f := range []struct{ path, content string }{
		{a, "original-a"},
		{b, "original-b"},
	} {
		if err := os.WriteFile(f.path, []byte(f.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	l, err := Begin(ws)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	txDir := l.dir

	// Record both files before any write.
	if err := l.Record(a, []byte("original-a"), 0o644); err != nil {
		t.Fatalf("Record a: %v", err)
	}
	if err := l.Record(b, []byte("original-b"), 0o644); err != nil {
		t.Fatalf("Record b: %v", err)
	}

	// Simulate: a was written, b write failed.
	if err := os.WriteFile(a, []byte("modified-a"), 0o644); err != nil {
		t.Fatal(err)
	}

	l.Rollback()

	if _, err := os.Stat(txDir); !os.IsNotExist(err) {
		t.Error("tx-log dir should be gone after Rollback")
	}
	if got, _ := os.ReadFile(a); string(got) != "original-a" {
		t.Errorf("a: got %q, want %q", got, "original-a")
	}
	if got, _ := os.ReadFile(b); string(got) != "original-b" {
		t.Errorf("b: got %q, want %q", got, "original-b")
	}
}

// TestRollback_SkipsUnsnapshottedOps verifies that Rollback skips ops with
// snapshotted=false (large files) without panicking.
func TestRollback_SkipsUnsnapshottedOps(t *testing.T) {
	ws := initWorkspace(t)
	target := filepath.Join(ws, "file.txt")
	if err := os.WriteFile(target, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Manually build a manifest with snapshotted=false — simulating a file
	// that exceeded maxSnapSize at Record time.
	txID := "unsnapshotted-test"
	txDir := filepath.Join(ws, ".plumb", "tx-log", txID)
	if err := os.MkdirAll(txDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := txManifest{
		TxID:      txID,
		Workspace: ws,
		Ops:       []opMeta{{N: 0, Path: target, Perm: 0o644, Snapshotted: false}},
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(txDir, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	// rollbackDir must log a warning and NOT touch the file.
	rollbackDir(txDir)

	got, _ := os.ReadFile(target)
	if string(got) != "current" {
		t.Errorf("file must not be touched for unsnapshotted op; got %q", got)
	}
}

// TestRollback_PermissionsPreserved verifies that Rollback writes the restored
// content with the permission bits recorded in the manifest.
func TestRollback_PermissionsPreserved(t *testing.T) {
	ws := initWorkspace(t)
	target := filepath.Join(ws, "script.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	l, err := Begin(ws)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := l.Record(target, []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Simulate a write that changed both content and permissions.
	if err := os.WriteFile(target, []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}

	l.Rollback()

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat after rollback: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("perm = %o, want %o", info.Mode().Perm(), 0o755)
	}
	if got, _ := os.ReadFile(target); string(got) != "#!/bin/sh" {
		t.Errorf("content after rollback = %q, want %q", got, "#!/bin/sh")
	}
}

// TestScan_MissingManifest verifies Scan handles an orphaned directory with no
// manifest.json (daemon crashed before Begin finished writing it).
func TestScan_MissingManifest(t *testing.T) {
	ws := initWorkspace(t)
	txDir := filepath.Join(ws, ".plumb", "tx-log", "crash-before-manifest")
	if err := os.MkdirAll(txDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No manifest.json written — Scan must not panic.
	Scan(ws, time.Now().Add(time.Hour)) // should log an error and continue
	// Directory should survive (Scan's RemoveAll only fires after rollbackDir).
	// Either outcome (removed or not) is acceptable — what we care about is no panic.
}

// TestScan_CorruptManifest verifies Scan handles a directory whose manifest
// contains invalid JSON (e.g. filesystem corruption) without panicking.
func TestScan_CorruptManifest(t *testing.T) {
	ws := initWorkspace(t)
	txDir := filepath.Join(ws, ".plumb", "tx-log", "corrupt-manifest")
	if err := os.MkdirAll(txDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(txDir, "manifest.json"), []byte("not valid json{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	Scan(ws, time.Now().Add(time.Hour)) // must not panic
}
