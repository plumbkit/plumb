package topology

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndexer_EnqueueOverflowFlagsResync(t *testing.T) {
	idx := &Indexer{queue: make(chan indexOp, 1)}
	idx.queue <- indexOp{kind: opUpsert, path: "first.go"} // fill the 1-slot buffer
	idx.Enqueue("second.go", opUpsert)                     // must overflow

	if !idx.takeResyncPending() {
		t.Error("expected resyncPending to be set after a queue overflow")
	}
	// takeResyncPending must have cleared the flag.
	if idx.takeResyncPending() {
		t.Error("resyncPending should be cleared after being taken")
	}
}

func TestIndexer_RunQueueCycle_HonoursPendingResync(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(DBPath(dir))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)

	// Simulate a dropped enqueue: the recovery flag is set but nothing queued.
	idx.mu.Lock()
	idx.resyncPending = true
	idx.mu.Unlock()

	// The cycle processes the (no-op) initial op, then must honour the pending
	// resync and reconcile the whole tree — indexing main.go.
	idx.runQueueCycle(indexOp{kind: opUpsert, path: ""})

	if idx.takeResyncPending() {
		t.Error("resyncPending should be cleared after runQueueCycle")
	}
	var nodes int
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_nodes`).Scan(&nodes)
	if nodes == 0 {
		t.Error("expected the recovery resync to index main.go, got 0 nodes")
	}
}
