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
