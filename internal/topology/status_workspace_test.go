package topology

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
	if _, err := os.Stat(DBPath(dir) + "-wal"); err == nil {
		t.Error("read-only status created a -wal sidecar")
	}
}
