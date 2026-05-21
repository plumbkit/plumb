package topology

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- shouldSkipDir ---

func TestShouldSkipDir_KnownDirs(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"vendor", true},
		{"node_modules", true},
		{"testdata", true},
		{"dist", true},
		{"build", true},
		{"__pycache__", true},
		{".git", true},
		{".vscode", true},
		{".idea", true},
		{".venv", true},
		{".next", true},
		{"internal", false},
		{"cmd", false},
		{"docs", false},
		{".", false},
		{".x", true},
	}
	for _, c := range cases {
		if got := shouldSkipDir(c.name); got != c.want {
			t.Errorf("shouldSkipDir(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- processUpsert / processDelete ---

func TestIndexer_ProcessUpsert_Insert(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	goFile := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024)
	if err := idx.processUpsert(context.Background(), "main.go"); err != nil {
		t.Fatalf("processUpsert: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM topology_nodes`).Scan(&count) //nolint:errcheck
	if count == 0 {
		t.Error("expected nodes after upsert, got 0")
	}
	var fileCount int
	db.QueryRow(`SELECT COUNT(*) FROM topology_files WHERE path = 'main.go'`).Scan(&fileCount) //nolint:errcheck
	if fileCount != 1 {
		t.Errorf("expected 1 file record, got %d", fileCount)
	}
}

func TestIndexer_ProcessUpsert_UpdateOnChange(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	goFile := filepath.Join(dir, "a.go")
	if err := os.WriteFile(goFile, []byte("package p\n\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024)
	if err := idx.processUpsert(context.Background(), "a.go"); err != nil {
		t.Fatalf("first processUpsert: %v", err)
	}

	var id1 int64
	db.QueryRow(`SELECT id FROM topology_files WHERE path = 'a.go'`).Scan(&id1) //nolint:errcheck

	// Modify the file so mtime changes.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(goFile, []byte("package p\n\nfunc Alpha() {}\n\nfunc Beta() {}\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// Touch mtime explicitly (some filesystems have 1s resolution).
	now := time.Now()
	if err := os.Chtimes(goFile, now, now.Add(time.Second)); err != nil {
		t.Logf("chtimes: %v (continuing)", err)
	}

	if err := idx.processUpsert(context.Background(), "a.go"); err != nil {
		t.Fatalf("second processUpsert: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM topology_nodes`).Scan(&count) //nolint:errcheck
	// Both Alpha and Beta should be present after the update.
	if count < 2 {
		t.Errorf("expected ≥2 nodes after update, got %d", count)
	}
}

func TestIndexer_ProcessUpsert_NotExistRoutesToDelete(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Insert a file record manually so processDelete has something to remove.
	fileID := insertTestFile(t, db, "gone.go")
	insertTestNode(t, db, fileID, "gone.go", Node{Kind: KindFunction, Name: "Gone", Language: "go"})

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024)
	// "gone.go" does not exist on disk — processUpsert should route to processDelete.
	if err := idx.processUpsert(context.Background(), "gone.go"); err != nil {
		t.Fatalf("processUpsert on missing file: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM topology_nodes`).Scan(&count) //nolint:errcheck
	if count != 0 {
		t.Errorf("expected 0 nodes after delete-via-upsert, got %d", count)
	}
}

func TestIndexer_ProcessDelete_RemovesNodesEdgesFTS(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "del.go")
	n1 := insertTestNode(t, db, fileID, "del.go", Node{Kind: KindFunction, Name: "Foo", Language: "go"})
	n2 := insertTestNode(t, db, fileID, "del.go", Node{Kind: KindFunction, Name: "Bar", Language: "go"})
	insertTestEdge(t, db, n1, n2, string(EdgeContains))

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024)
	if err := idx.processDelete(context.Background(), "del.go"); err != nil {
		t.Fatalf("processDelete: %v", err)
	}

	var nodes, edges, ftsRows, files int
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_nodes`).Scan(&nodes)
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_edges`).Scan(&edges)
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_fts`).Scan(&ftsRows)
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_files`).Scan(&files)
	if nodes != 0 {
		t.Errorf("nodes not deleted: got %d", nodes)
	}
	if edges != 0 {
		t.Errorf("edges not deleted: got %d", edges)
	}
	if ftsRows != 0 {
		t.Errorf("FTS rows not deleted: got %d", ftsRows)
	}
	if files != 0 {
		t.Errorf("file record not deleted: got %d", files)
	}
}

func TestIndexer_ProcessDelete_MissingFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024)
	// Deleting a path not in the DB should be a no-op (no error).
	if err := idx.processDelete(context.Background(), "nonexistent.go"); err != nil {
		t.Errorf("processDelete on unknown path: %v", err)
	}
}

// --- isStale ---

func TestIndexer_IsStale_NewFile(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	goFile := filepath.Join(dir, "s.go")
	if err := os.WriteFile(goFile, []byte("package p"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, _ := os.Stat(goFile)
	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024)

	stale, fileID, err := idx.isStale("s.go", info)
	if err != nil {
		t.Fatalf("isStale: %v", err)
	}
	if !stale {
		t.Error("expected stale=true for unknown file")
	}
	if fileID != 0 {
		t.Errorf("expected fileID=0 for new file, got %d", fileID)
	}
}

func TestIndexer_IsStale_UnchangedFile(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	goFile := filepath.Join(dir, "u.go")
	if err := os.WriteFile(goFile, []byte("package p"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, _ := os.Stat(goFile)
	// Insert a file record with the same mtime.
	db.Exec( //nolint:errcheck
		`INSERT INTO topology_files(path, mtime_ns, content_hash, indexed_at, error_msg)
         VALUES (?, ?, '', 0, '')`, "u.go", info.ModTime().UnixNano())

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024)
	stale, _, err := idx.isStale("u.go", info)
	if err != nil {
		t.Fatalf("isStale: %v", err)
	}
	if stale {
		t.Error("expected stale=false for file with matching mtime")
	}
}

// --- safeExtract panic recovery ---

type panicExtractor struct{}

func (p *panicExtractor) Language() string     { return "go" }
func (p *panicExtractor) Extensions() []string { return []string{".go"} }
func (p *panicExtractor) Extract(_ context.Context, _ string, _ []byte) ([]Node, []Edge, error) {
	panic("intentional extractor panic")
}

func TestSafeExtract_PanicRecovery(t *testing.T) {
	ex := &panicExtractor{}
	nodes, edges, err := safeExtract(context.Background(), ex, "test.go", []byte("package p"))
	if err == nil {
		t.Error("expected error from panic recovery, got nil")
	}
	if nodes != nil || edges != nil {
		t.Error("expected nil nodes/edges after panic")
	}
}

// --- recordFileError ---

func TestIndexer_RecordFileError(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	goFile := filepath.Join(dir, "bad.go")
	if err := os.WriteFile(goFile, []byte("package p"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, _ := os.Stat(goFile)
	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024)

	fakeErr := os.ErrInvalid
	if err := idx.recordFileError("bad.go", info, fakeErr); err != nil {
		t.Fatalf("recordFileError: %v", err)
	}

	var errMsg string
	db.QueryRow(`SELECT error_msg FROM topology_files WHERE path = 'bad.go'`).Scan(&errMsg) //nolint:errcheck
	if errMsg == "" {
		t.Error("expected error_msg to be set after recordFileError")
	}
}

// --- drain coalescing ---

func TestIndexer_Drain_ReturnsLast(t *testing.T) {
	idx := &Indexer{
		queue: make(chan indexOp, 256),
	}
	// Pre-fill the queue with three ops.
	idx.queue <- indexOp{kind: opUpsert, path: "a.go"}
	idx.queue <- indexOp{kind: opUpsert, path: "b.go"}
	idx.queue <- indexOp{kind: opResync, path: ""}

	// drain should return the last op (opResync).
	result := idx.drain(indexOp{kind: opUpsert, path: "initial.go"})
	if result.kind != opResync {
		t.Errorf("drain returned kind=%v, want opResync", result.kind)
	}
}

// --- pruneDeleted ---

func TestIndexer_PruneDeleted_RemovesStale(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Insert a file that is NOT in the "present" set.
	insertTestFile(t, db, "stale.go")

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024)
	present := map[string]bool{} // empty — "stale.go" is absent
	if err := idx.pruneDeleted(present); err != nil {
		t.Fatalf("pruneDeleted: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM topology_files`).Scan(&count) //nolint:errcheck
	if count != 0 {
		t.Errorf("expected 0 file records after prune, got %d", count)
	}
}

func TestIndexer_PruneDeleted_KeepsPresent(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	insertTestFile(t, db, "keep.go")
	insertTestFile(t, db, "remove.go")

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024)
	present := map[string]bool{"keep.go": true}
	if err := idx.pruneDeleted(present); err != nil {
		t.Fatalf("pruneDeleted: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM topology_files`).Scan(&count) //nolint:errcheck
	if count != 1 {
		t.Errorf("expected 1 remaining file record, got %d", count)
	}
	var path string
	db.QueryRow(`SELECT path FROM topology_files`).Scan(&path) //nolint:errcheck
	if path != "keep.go" {
		t.Errorf("remaining file = %q, want keep.go", path)
	}
}

// --- FTS path invariant after processDelete ---

func TestFTSPathInvariant_AfterDelete(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "sync.go")
	insertTestNode(t, db, fileID, "sync.go", Node{Kind: KindFunction, Name: "Sync", Language: "go"})

	// Verify FTS row exists before delete.
	var ftsCount int
	db.QueryRow(`SELECT COUNT(*) FROM topology_fts WHERE path = 'sync.go'`).Scan(&ftsCount) //nolint:errcheck
	if ftsCount == 0 {
		t.Fatal("FTS row missing before delete")
	}

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024)
	if err := idx.processDelete(context.Background(), "sync.go"); err != nil {
		t.Fatalf("processDelete: %v", err)
	}

	// FTS row must be gone after delete.
	db.QueryRow(`SELECT COUNT(*) FROM topology_fts WHERE path = 'sync.go'`).Scan(&ftsCount) //nolint:errcheck
	if ftsCount != 0 {
		t.Errorf("FTS row still present after processDelete: got %d", ftsCount)
	}
}
