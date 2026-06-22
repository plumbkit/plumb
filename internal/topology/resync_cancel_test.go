package topology

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// With pacing DISABLED (resyncBatch/resyncPause == 0) a resync must still abort
// promptly on shutdown. pace() is the only place the walk observed idx.done/ctx,
// and it is a no-op when pacing is off — so without an unconditional check at the
// top of the walk callback, Stop() (close(done) then wg.Wait()) would block for
// the entire walk of a large workspace. Regression for #65 part 2 (shutdown hang).
func TestProcessResync_PacingDisabled_AbortsOnDone(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(DBPath(dir))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	for _, name := range []string{"a.go", "b.go", "c.go", "d.go"} {
		body := "package p\n\nfunc F" + name[:1] + "() {}\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	idx.resyncBatch = 0 // pacing disabled — pace() never observes cancellation
	idx.resyncPause = 0

	close(idx.done) // Stop() has been called before/at the start of the walk

	if err := idx.processResync(context.Background()); err != nil {
		t.Fatalf("an aborted resync should return nil (skip prune), got %v", err)
	}

	var files int
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_files`).Scan(&files)
	if files != 0 {
		t.Errorf("a pacing-disabled resync must abort before indexing once done is closed; indexed %d files", files)
	}
}

// Same guarantee via context cancellation rather than Stop().
func TestProcessResync_PacingDisabled_AbortsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(DBPath(dir))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	for _, name := range []string{"a.go", "b.go", "c.go", "d.go"} {
		body := "package p\n\nfunc F" + name[:1] + "() {}\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	idx.resyncBatch = 0
	idx.resyncPause = 0

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := idx.processResync(ctx); err != nil {
		t.Fatalf("an aborted resync should return nil (skip prune), got %v", err)
	}

	var files int
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_files`).Scan(&files)
	if files != 0 {
		t.Errorf("a pacing-disabled resync must abort before indexing once ctx is cancelled; indexed %d files", files)
	}
}
