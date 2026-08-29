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
	var out extractOutput
	var err error
	go func() {
		out, err = safeExtract(ctx, ex, "stuck.go", []byte("package p"))
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
	if out.nodes != nil || out.edges != nil {
		t.Error("expected no nodes/edges from an abandoned extract")
	}
	<-ex.entered // the extractor really did start; the timeout is not a no-op path
}

func TestSafeExtract_ReturnsWorkThatFitsTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := safeExtract(ctx, &slowExtractor{delay: 20 * time.Millisecond}, "slow.go", []byte("package p"))
	if err != nil {
		t.Fatalf("safeExtract: %v", err)
	}
	if len(out.nodes) != 1 || out.nodes[0].Name != "Slow" {
		t.Errorf("nodes = %+v, want the single Slow node — a deadline that fits must not truncate", out.nodes)
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

// A configured 0 no longer means "no deadline" — it resolves to the
// maxExtractTimeout ceiling. What it must still guarantee is that an ordinary
// parse is not abandoned: the ceiling is far above any legitimate single-file
// parse, so a 20ms extractor completes and records no error.
func TestIndexer_ExtractTimeoutZero_UsesTheCeilingAndDoesNotAbandon(t *testing.T) {
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
	idx.extractTimeout = 0 // resolves to maxExtractTimeout, not "unbounded"

	if err := idx.processUpsert(context.Background(), "slow.go"); err != nil {
		t.Fatalf("processUpsert: %v", err)
	}

	var msg string
	if err := db.QueryRow(`SELECT error_msg FROM topology_files WHERE path = ?`, "slow.go").Scan(&msg); err != nil {
		t.Fatalf("query file row: %v", err)
	}
	if msg != "" {
		t.Errorf("error_msg = %q, want empty — a parse well inside the ceiling must never be abandoned", msg)
	}

	if got := effectiveExtractTimeout(0); got != maxExtractTimeout {
		t.Errorf("effectiveExtractTimeout(0) = %v, want the %v ceiling — 0 must not mean unbounded", got, maxExtractTimeout)
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

// armedContext reports itself cancelled through Err() from the outset but only
// closes Done() once the extractor has been entered. That inversion is illegal
// for a real context and deliberate here: it is what makes the test below
// scheduler-independent. In an implementation that spawns the extract before
// checking Err(), safeExtract's select has exactly one reachable arm and cannot
// return until the extract has started — so observing afterwards that it did
// NOT start is a fact about the implementation, not a sample of the scheduler.
type armedContext struct{ entered chan struct{} }

func (armedContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c armedContext) Done() <-chan struct{}     { return c.entered }
func (armedContext) Err() error                  { return context.Canceled }
func (armedContext) Value(any) any               { return nil }

// enteringExtractor closes entered as its first act, so entering it is what
// arms the context above.
type enteringExtractor struct{ entered chan struct{} }

func (enteringExtractor) Language() string     { return "go" }
func (enteringExtractor) Extensions() []string { return []string{".go"} }

func (e enteringExtractor) Extract(_ context.Context, relPath string, _ []byte) ([]Node, []Edge, error) {
	close(e.entered)
	return []Node{{Path: relPath, Name: "Fast", Kind: KindFunction, Language: "go"}}, nil, nil
}

func TestSafeExtract_DeadContextStartsNoExtract(t *testing.T) {
	entered := make(chan struct{})
	ctx := armedContext{entered: entered}

	out, err := safeExtract(ctx, enteringExtractor{entered: entered}, "dead.go", []byte("package p"))

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want a context.Canceled wrapper", err)
	}
	if out.nodes != nil || out.edges != nil {
		t.Errorf("nodes/edges = %v/%v, want none — a dead context must not return a result", out.nodes, out.edges)
	}
	select {
	case <-entered:
		t.Fatal("a dead context still started an extract; the watchdog spawned work it had already given up on")
	default:
	}
}

// TestEffectiveExtractTimeout_CeilingCannotBeRemoved pins the bound that stops a
// pathological file wedging the single indexer worker.
//
// "Disabled" used to mean literally unbounded: no context deadline, so no parser
// timeout, and a watchdog waiting on a ctx.Done() that never arrives. The
// configured value may lower the bound — that is tuning — but 0 and
// absurdly-large values both resolve to the ceiling, because an index that
// silently stops is a worse outcome than a file that is skipped and retried.
func TestEffectiveExtractTimeout_CeilingCannotBeRemoved(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{"the default is honoured", 10 * time.Second, 10 * time.Second},
		{"a tighter value is honoured", 500 * time.Millisecond, 500 * time.Millisecond},
		{"zero means the ceiling, not unbounded", 0, maxExtractTimeout},
		{"negative means the ceiling", -1 * time.Second, maxExtractTimeout},
		{"a value above the ceiling is clamped", time.Hour, maxExtractTimeout},
		{"exactly the ceiling is honoured", maxExtractTimeout, maxExtractTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveExtractTimeout(tc.configured); got != tc.want {
				t.Errorf("effectiveExtractTimeout(%v) = %v, want %v", tc.configured, got, tc.want)
			}
		})
	}
}

// A parse must be bounded even when the operator disabled the timeout: with
// extractTimeout 0 the context handed to the extractor still carries a deadline.
func TestExtractFile_DeadlineAppliedEvenWhenTimeoutDisabled(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	var sawDeadline bool
	probe := &deadlineProbeExtractor{onExtract: func(ctx context.Context) {
		_, sawDeadline = ctx.Deadline()
	}}
	idx := newIndexer(dir, db, []Extractor{probe}, 512*1024, 0) // 0 == "disabled"
	if _, err := idx.extractFile(context.Background(), probe, "a.go", []byte("package main\n")); err != nil {
		t.Fatalf("extractFile: %v", err)
	}
	if !sawDeadline {
		t.Error("the extractor received a context with no deadline; a disabled timeout must still be bounded by the ceiling")
	}
}

// deadlineProbeExtractor reports the context it was handed.
type deadlineProbeExtractor struct{ onExtract func(context.Context) }

func (e *deadlineProbeExtractor) Language() string     { return "go" }
func (e *deadlineProbeExtractor) Extensions() []string { return []string{".go"} }
func (e *deadlineProbeExtractor) Extract(ctx context.Context, _ string, _ []byte) ([]Node, []Edge, error) {
	e.onExtract(ctx)
	return nil, nil, nil
}
