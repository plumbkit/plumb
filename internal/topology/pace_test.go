package topology

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPace_DisabledByZeroBatch(t *testing.T) {
	idx := &Indexer{done: make(chan struct{}), resyncBatch: 0, resyncPause: 50 * time.Millisecond}
	start := time.Now()
	if err := idx.pace(context.Background(), 100); err != nil {
		t.Fatalf("pace: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Errorf("expected no pause when batch=0, slept %v", elapsed)
	}
}

func TestPace_PausesOnBatchBoundary(t *testing.T) {
	idx := &Indexer{done: make(chan struct{}), resyncBatch: 2, resyncPause: 30 * time.Millisecond}

	// Off the boundary: no pause.
	start := time.Now()
	if err := idx.pace(context.Background(), 1); err != nil {
		t.Fatalf("pace(1): %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Errorf("expected no pause off the batch boundary, slept %v", elapsed)
	}

	// On the boundary: pauses ~resyncPause.
	start = time.Now()
	if err := idx.pace(context.Background(), 2); err != nil {
		t.Fatalf("pace(2): %v", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("expected a ~30ms pause on the batch boundary, slept %v", elapsed)
	}
}

func TestPace_AbortsOnDone(t *testing.T) {
	idx := &Indexer{done: make(chan struct{}), resyncBatch: 1, resyncPause: time.Hour}
	close(idx.done)
	if err := idx.pace(context.Background(), 1); !errors.Is(err, errResyncAborted) {
		t.Errorf("expected errResyncAborted when done is closed, got %v", err)
	}
}

func TestPace_AbortsOnContextCancel(t *testing.T) {
	idx := &Indexer{done: make(chan struct{}), resyncBatch: 1, resyncPause: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := idx.pace(ctx, 1); !errors.Is(err, errResyncAborted) {
		t.Errorf("expected errResyncAborted on context cancel, got %v", err)
	}
}

// A paced resync must still index every file and prune as usual.
func TestProcessResync_WithPacingIndexesAllFiles(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(DBPath(dir))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	for _, name := range []string{"a.go", "b.go", "c.go"} {
		body := "package p\n\nfunc F" + name[:1] + "() {}\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	idx.resyncBatch = 1                    // pace after every file
	idx.resyncPause = 1 * time.Millisecond // keep the test fast

	if err := idx.processResync(context.Background()); err != nil {
		t.Fatalf("processResync: %v", err)
	}

	var files int
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_files`).Scan(&files)
	if files < 3 {
		t.Errorf("expected >=3 files indexed under pacing, got %d", files)
	}
}
