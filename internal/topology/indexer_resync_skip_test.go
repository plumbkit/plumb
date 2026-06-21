package topology

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// countingExtractor wraps minimalExtractor and counts each Extract call, so a
// test can prove the parse runs only when a file is actually stale.
type countingExtractor struct {
	minimalExtractor
	calls atomic.Int64
}

func (c *countingExtractor) Extract(ctx context.Context, relPath string, src []byte) ([]Node, []Edge, error) {
	c.calls.Add(1)
	return c.minimalExtractor.Extract(ctx, relPath, src)
}

// TestIndexer_ProcessUpsert_SkipsParseWhenUnchanged is the regression test for
// issue #58: a resync of an up-to-date tree must not re-parse files whose mtime
// and content hash already match the index. The parse is the expensive step, so
// the staleness check has to gate it. A genuinely changed file must still parse.
func TestIndexer_ProcessUpsert_SkipsParseWhenUnchanged(t *testing.T) {
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

	ext := &countingExtractor{}
	idx := newIndexer(dir, db, []Extractor{ext}, 512*1024, 0)

	// First index: the file is new, so it must parse.
	if err := idx.processUpsert(context.Background(), "a.go"); err != nil {
		t.Fatalf("first processUpsert: %v", err)
	}
	if got := ext.calls.Load(); got != 1 {
		t.Fatalf("expected 1 parse on first index, got %d", got)
	}

	// Second index of the UNCHANGED file (the resync case): the staleness check
	// must short-circuit before the parse, so the call count stays at 1.
	if err := idx.processUpsert(context.Background(), "a.go"); err != nil {
		t.Fatalf("resync processUpsert: %v", err)
	}
	if got := ext.calls.Load(); got != 1 {
		t.Fatalf("unchanged file was re-parsed: expected 1 parse total, got %d", got)
	}

	// Changing the file's content and mtime must re-trigger the parse.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(goFile, []byte("package p\n\nfunc Alpha() {}\n\nfunc Beta() {}\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	now := time.Now()
	if err := os.Chtimes(goFile, now, now.Add(time.Second)); err != nil {
		t.Logf("chtimes: %v (continuing)", err)
	}
	if err := idx.processUpsert(context.Background(), "a.go"); err != nil {
		t.Fatalf("changed processUpsert: %v", err)
	}
	if got := ext.calls.Load(); got != 2 {
		t.Fatalf("changed file was not re-parsed: expected 2 parses total, got %d", got)
	}
}
