package topology

import (
	"context"
	"errors"
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
