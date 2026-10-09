package topology

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// injectedIOErr is the message the fault-injection triggers raise. It mirrors
// the live PLAN-467 failure ("disk I/O error (8714)") so a reader of a failing
// run sees which error class is being simulated.
const injectedIOErr = "disk I/O error (injected)"

// failDerivedRebuild makes every write to topology_meta raise. Only the
// derived-edge rebuild writes there (the call resolver's bookkeeping, then the
// resolver fingerprint), so the cycle fails in the phase the live PLAN-467
// index failed in ("link imports: clear: disk I/O error (8714)") while, as
// there, per-file indexing keeps working.
func failDerivedRebuild(t *testing.T, idx *Indexer) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TRIGGER inject_meta_insert BEFORE INSERT ON topology_meta BEGIN SELECT RAISE(ABORT, '` + injectedIOErr + `'); END`,
		`CREATE TRIGGER inject_meta_update BEFORE UPDATE ON topology_meta BEGIN SELECT RAISE(ABORT, '` + injectedIOErr + `'); END`,
	} {
		if _, err := idx.db.Exec(stmt); err != nil {
			t.Fatalf("install fault trigger: %v", err)
		}
	}
}

func healDerivedRebuild(t *testing.T, idx *Indexer) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TRIGGER inject_meta_insert`,
		`DROP TRIGGER inject_meta_update`,
	} {
		if _, err := idx.db.Exec(stmt); err != nil {
			t.Fatalf("drop fault trigger: %v", err)
		}
	}
}

// TestIndexer_FailedCycleRetriesUntilItRecovers is the PLAN-467 regression: a
// cycle that fails in the derived-edge rebuild must be retried on its own,
// with no file event and no periodic resync, and the index must report itself
// failing until a retry succeeds.
//
// The indexer runs as it does under the file watcher, where the periodic
// resync is off (resyncMins 0). Before the retry timer, nothing re-ran a failed
// cycle there except the next file event, so a quiet workspace stayed in error
// indefinitely — the live index sat in error for most of a day.
func TestIndexer_FailedCycleRetriesUntilItRecovers(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatalf("write a.go: %v", err)
	}

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	idx.retryBase = 20 * time.Millisecond
	idx.retryMax = 80 * time.Millisecond
	failDerivedRebuild(t, idx)
	idx.Start() // the startup resync is the cycle that fails
	t.Cleanup(idx.Stop)

	if !waitFor(func() bool { return idx.Health().Failing }, 5*time.Second) {
		t.Fatalf("startup cycle did not fail under the injected fault; health = %+v", idx.Health())
	}
	h := idx.Health()
	if !strings.Contains(h.LastError, injectedIOErr) {
		t.Errorf("LastError = %q, want it to carry the injected error", h.LastError)
	}
	if !h.LastSync.IsZero() {
		t.Errorf("LastSync = %v after only failed cycles, want zero", h.LastSync)
	}
	if s := Report(db, dir, idx); !s.Failing || !strings.Contains(FormatStatus(s, dir), "FAILING:") {
		t.Errorf("topology_status's snapshot does not flag the failing index: Failing=%v", s.Failing)
	}

	// Positive control for the retry itself: while the fault holds, retries keep
	// failing and the index keeps saying so. A Failing that cleared here would
	// mean something other than a successful cycle cleared it.
	time.Sleep(200 * time.Millisecond) // several retryMax periods
	if h := idx.Health(); !h.Failing {
		t.Fatalf("index stopped reporting failure while the fault still holds; health = %+v", h)
	}

	// Remove the fault and enqueue NOTHING: only the retry timer can recover.
	healDerivedRebuild(t, idx)
	if !waitFor(func() bool {
		h := idx.Health()
		return !h.Failing && h.State == "idle"
	}, 5*time.Second) {
		t.Fatalf("index did not recover after the fault cleared; health = %+v", idx.Health())
	}
	h = idx.Health()
	if h.LastSync.IsZero() {
		t.Error("LastSync is still zero after a successful retry")
	}
	if !strings.Contains(h.LastError, injectedIOErr) {
		t.Errorf("LastError = %q after recovery, want the historical error kept", h.LastError)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM topology_meta`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("recovered cycle did not write the resolver fingerprint")
	}
}

// TestIndexer_FailingSpansARetryCycle pins the setState contract the tools'
// stale-index notice relies on: a cycle in flight after a failure is still
// failing, and only a cycle that ends idle clears it.
func TestIndexer_FailingSpansARetryCycle(t *testing.T) {
	idx := newIndexer(t.TempDir(), nil, nil, 0, 0)
	if idx.Health().Failing {
		t.Fatal("a new indexer reports failing")
	}
	idx.setState("error", "boom")
	if !idx.Health().Failing {
		t.Fatal(`setState("error") did not mark the index failing`)
	}
	idx.setState("running", "")
	if h := idx.Health(); !h.Failing || h.State != "running" {
		t.Fatalf("a retry cycle in flight cleared the failure; health = %+v", h)
	}
	idx.setState("idle", "")
	if h := idx.Health(); h.Failing || h.LastSync.IsZero() {
		t.Fatalf("a successful cycle did not clear the failure and stamp LastSync; health = %+v", h)
	}
}

func TestNextRetryDelay(t *testing.T) {
	const base, limit = time.Second, 5 * time.Second
	tests := []struct {
		name        string
		prev, limit time.Duration
		want        time.Duration
	}{
		{"first retry starts at base", 0, limit, base},
		{"doubles", base, limit, 2 * base},
		{"doubles again", 2 * base, limit, 4 * base},
		{"capped at limit", 4 * base, limit, limit},
		{"stays at limit", limit, limit, limit},
		{"zero limit is uncapped", 8 * base, 0, 16 * base},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextRetryDelay(tt.prev, base, tt.limit); got != tt.want {
				t.Errorf("nextRetryDelay(%v, %v, %v) = %v, want %v", tt.prev, base, tt.limit, got, tt.want)
			}
		})
	}
}

func TestFormatStatus_FailingIsFlagged(t *testing.T) {
	healthy := FormatStatus(Status{IndexerState: "idle", LastError: "old error"}, "/ws")
	if strings.Contains(healthy, "FAILING") {
		t.Errorf("a healthy index with a historical error is flagged failing:\n%s", healthy)
	}
	failing := FormatStatus(Status{IndexerState: "running", LastError: "disk I/O error", Failing: true}, "/ws")
	if !strings.Contains(failing, "FAILING:") {
		t.Errorf("a failing index is not flagged:\n%s", failing)
	}
}

func TestStoreHealth_NoIndexerIsStopped(t *testing.T) {
	if h := (&Store{}).Health(); h.State != "stopped" || h.Failing {
		t.Errorf("Health() with no indexer = %+v, want stopped and not failing", h)
	}
}
