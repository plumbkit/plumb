package topology

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests pin the per-file extract deadline. Without it a grammar whose
// error recovery goes superlinear on one file stalls the indexer's single
// background worker, and with it the whole workspace's index: every later
// upsert queues behind that one parse. See safeExtract.

// blockingExtractor never returns. It stands in for a parse that ignores the
// context entirely — the case the watchdog exists for.
type blockingExtractor struct {
	entered chan struct{} // closed once Extract has been called
	release chan struct{} // closed by the test to let the goroutine exit
}

func newBlockingExtractor() *blockingExtractor {
	return &blockingExtractor{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingExtractor) Language() string     { return "go" }
func (b *blockingExtractor) Extensions() []string { return []string{".go"} }

func (b *blockingExtractor) Extract(_ context.Context, _ string, _ []byte) ([]Node, []Edge, error) {
	close(b.entered)
	<-b.release
	return nil, nil, nil
}

// slowExtractor finishes, but only after delay. It proves the deadline does not
// truncate legitimate work that fits inside it.
type slowExtractor struct{ delay time.Duration }

func (s *slowExtractor) Language() string     { return "go" }
func (s *slowExtractor) Extensions() []string { return []string{".go"} }

func (s *slowExtractor) Extract(_ context.Context, relPath string, _ []byte) ([]Node, []Edge, error) {
	time.Sleep(s.delay)
	return []Node{{Path: relPath, Name: "Slow", Kind: KindFunction, Language: "go"}}, nil, nil
}

func TestSafeExtract_AbandonsExtractPastTheDeadline(t *testing.T) {
	ex := newBlockingExtractor()
	defer close(ex.release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	var nodes []Node
	var edges []Edge
	var err error
	go func() {
		nodes, edges, err = safeExtract(ctx, ex, "stuck.go", []byte("package p"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("safeExtract did not return after its context expired — the worker is wedged")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a context.DeadlineExceeded wrapper", err)
	}
	if nodes != nil || edges != nil {
		t.Error("expected no nodes/edges from an abandoned extract")
	}
	<-ex.entered // the extractor really did start; the timeout is not a no-op path
}

func TestSafeExtract_ReturnsWorkThatFitsTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	nodes, _, err := safeExtract(ctx, &slowExtractor{delay: 20 * time.Millisecond}, "slow.go", []byte("package p"))
	if err != nil {
		t.Fatalf("safeExtract: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "Slow" {
		t.Errorf("nodes = %+v, want the single Slow node — a deadline that fits must not truncate", nodes)
	}
}

func TestIndexer_ExtractTimeout_RecordsErrorAndMovesOn(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	if err := os.WriteFile(filepath.Join(dir, "stuck.go"), []byte("package p"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	ex := newBlockingExtractor()
	defer close(ex.release)

	idx := newIndexer(dir, db, []Extractor{ex}, 512*1024, 0)
	idx.extractTimeout = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- idx.processUpsert(context.Background(), "stuck.go") }()

	select {
	case upErr := <-done:
		// processUpsert swallows the extract error into a recorded file row.
		if upErr != nil {
			t.Fatalf("processUpsert: %v", upErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("processUpsert never returned — a slow parse stalls the indexer worker")
	}

	var msg string
	if err := db.QueryRow(`SELECT error_msg FROM topology_files WHERE path = ?`, "stuck.go").Scan(&msg); err != nil {
		t.Fatalf("query file row: %v", err)
	}
	if msg == "" {
		t.Error("expected the abandoned file to be recorded with an error, got an empty error_msg")
	}
}

func TestIndexer_ExtractTimeoutZero_DisablesTheDeadline(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	if err := os.WriteFile(filepath.Join(dir, "slow.go"), []byte("package p"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	idx := newIndexer(dir, db, []Extractor{&slowExtractor{delay: 20 * time.Millisecond}}, 512*1024, 0)
	idx.extractTimeout = 0 // disabled

	if err := idx.processUpsert(context.Background(), "slow.go"); err != nil {
		t.Fatalf("processUpsert: %v", err)
	}

	var msg string
	if err := db.QueryRow(`SELECT error_msg FROM topology_files WHERE path = ?`, "slow.go").Scan(&msg); err != nil {
		t.Fatalf("query file row: %v", err)
	}
	if msg != "" {
		t.Errorf("error_msg = %q, want empty — a disabled timeout must never abandon a parse", msg)
	}
}

// failingExtractor returns the error shape safeExtract produces when it
// abandons a parse at the deadline — deterministically, with no real timeout
// and no goroutine to schedule.
type failingExtractor struct{}

func (failingExtractor) Language() string     { return "go" }
func (failingExtractor) Extensions() []string { return []string{".go"} }

func (failingExtractor) Extract(_ context.Context, relPath string, _ []byte) ([]Node, []Edge, error) {
	return nil, nil, fmt.Errorf("extract %s: %w", relPath, context.DeadlineExceeded)
}

// TestIndexer_TimedOutTouchedFileIsRetried pins the retry contract for the one
// shape that used to escape it: a file that was indexed cleanly, then touched
// without a byte changing, then failed to extract.
//
// recordFileError deliberately stores no content hash so the staleness check
// re-attempts the file on the next cycle — but its ON CONFLICT clause updated
// only mtime and error_msg, so the hash left by the earlier SUCCESSFUL index
// survived. With the mtime now current and the hash still matching the
// unchanged bytes, isStale said "fresh" and the file was never re-attempted:
// one transient timeout retired it from indexing until its content changed.
func TestIndexer_TimedOutTouchedFileIsRetried(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	abs := filepath.Join(dir, "a.go")
	if err := os.WriteFile(abs, []byte("package p"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	good := newIndexer(dir, db, []Extractor{&slowExtractor{}}, 512*1024, 0)
	if err := good.processUpsert(context.Background(), "a.go"); err != nil {
		t.Fatalf("first index: %v", err)
	}

	// Touch it: a new mtime, byte-identical content — a `git checkout` of the
	// same revision, a formatter that changed nothing, a restored backup.
	touched := time.Now().Add(time.Minute)
	if err := os.Chtimes(abs, touched, touched); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	bad := newIndexer(dir, db, []Extractor{failingExtractor{}}, 512*1024, 0)
	if err := bad.processUpsert(context.Background(), "a.go"); err != nil {
		t.Fatalf("failing index: %v", err)
	}
	if msg := fileErrorMsg(t, db, "a.go"); msg == "" {
		t.Fatal("the failed extract was not recorded; the rest of this test would be vacuous")
	}

	// Nothing about the file changes now — same mtime, same bytes. The only
	// thing that can make it stale again is the record the error path left.
	if err := good.processUpsert(context.Background(), "a.go"); err != nil {
		t.Fatalf("retry index: %v", err)
	}
	if msg := fileErrorMsg(t, db, "a.go"); msg != "" {
		t.Errorf("error_msg = %q after the retry cycle, want empty — a touched-but-identical file "+
			"that timed out once is never re-attempted", msg)
	}
}

func fileErrorMsg(t *testing.T, db *sql.DB, relPath string) string {
	t.Helper()
	var msg string
	if err := db.QueryRow(`SELECT error_msg FROM topology_files WHERE path = ?`, relPath).Scan(&msg); err != nil {
		t.Fatalf("query file row: %v", err)
	}
	return msg
}
