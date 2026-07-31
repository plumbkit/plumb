package topology

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

func TestStatusForWorkspace_MissingDB(t *testing.T) {
	_, err := StatusForWorkspace(t.TempDir())
	if err == nil {
		t.Fatal("expected an error for a missing topology DB")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected an os.IsNotExist error, got %v", err)
	}
}

func TestStatusForWorkspace_Populated(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(DBPath(dir))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	if err := idx.processUpsert(context.Background(), "main.go"); err != nil {
		t.Fatalf("processUpsert: %v", err)
	}
	db.Close()

	st, err := StatusForWorkspace(dir)
	if err != nil {
		t.Fatalf("StatusForWorkspace: %v", err)
	}
	if st.TotalNodes == 0 {
		t.Error("expected TotalNodes > 0 for a populated index")
	}
	if st.IndexedFiles == 0 {
		t.Error("expected IndexedFiles > 0 for a populated index")
	}
	if st.IndexerState != "stopped" {
		t.Errorf("IndexerState = %q, want \"stopped\" (no live indexer)", st.IndexerState)
	}
}

// TestStatusForWorkspace_ReadOnly asserts the inspection is side-effect-free:
// reading the index neither mutates the main DB file nor creates a -wal sidecar.
func TestStatusForWorkspace_ReadOnly(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(DBPath(dir))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	if err := idx.processUpsert(context.Background(), "main.go"); err != nil {
		t.Fatalf("processUpsert: %v", err)
	}
	db.Close()

	// Clear any sidecars the writer left so the assertion isolates what the
	// read-only open does. A clean close checkpoints the WAL into the main DB,
	// so removing the (now-redundant) sidecars loses no data.
	_ = os.Remove(DBPath(dir) + "-wal")
	_ = os.Remove(DBPath(dir) + "-shm")

	before, err := os.Stat(DBPath(dir))
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	if _, err := StatusForWorkspace(dir); err != nil {
		t.Fatalf("StatusForWorkspace: %v", err)
	}

	after, err := os.Stat(DBPath(dir))
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("read-only status mutated the DB file: mtime %v -> %v", before.ModTime(), after.ModTime())
	}
	// Deliberately NOT asserting the absence of -wal/-shm sidecars. That used
	// to hold, but only because `mode=ro` on a bare path is ignored, so the
	// handle was never actually read-only and SQLite had no reason to build a
	// WAL index. A genuinely read-only reader of a WAL database does create
	// them, and they are transient bookkeeping the .gitignore already covers.
	// The guarantee that matters is the mtime check above: the main database is
	// not mutated.
}

// TestStatusForWorkspace_ReadHandleIsTimedAndReadOnly pins both halves of the
// status read handle.
//
// busy_timeout guards the mattn-style `_busy_timeout=` regression, which the
// modernc driver silently ignores. The write refusal guards a second, subtler
// one found while consolidating these opens: `mode=ro` is honoured only when
// the DSN is a file: URI. Appended to a bare path it is read as part of the
// filename, and the handle this function documents as side-effect-free opens
// read-WRITE.
func TestStatusForWorkspace_ReadHandleIsTimedAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(DBPath(dir))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	db.Close()

	ro, err := sqlitex.OpenReadOnly(DBPath(dir), sqlitex.ReadOnlyOptions{BusyTimeout: statusReadTimeout})
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	defer ro.Close()

	var bt int
	if err := ro.QueryRow("PRAGMA busy_timeout").Scan(&bt); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if bt != int(statusReadTimeout.Milliseconds()) {
		t.Errorf("busy_timeout = %d, want %d", bt, statusReadTimeout.Milliseconds())
	}

	if _, err := ro.Exec(`INSERT INTO topology_files (path, lang, mtime, size, hash) VALUES ('x','go',0,0,'h')`); err == nil {
		t.Error("insert succeeded on the status read handle — it is not actually read-only")
	}
}
