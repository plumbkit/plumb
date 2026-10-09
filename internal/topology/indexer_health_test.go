package topology

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	// A full resync drains the arena pool as its last step, and the per-file
	// half of each retry succeeds under this fault, so reclaims count retries.
	var resyncs atomic.Int64
	idx.reclaimFn = func() { resyncs.Add(1) }
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
	// running and failing, and the index keeps saying so.
	before := resyncs.Load()
	time.Sleep(200 * time.Millisecond) // several retryMax periods
	if got := resyncs.Load() - before; got < 2 {
		t.Fatalf("%d retry resyncs ran in 200ms with a 20-80ms backoff, want at least 2", got)
	}
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

// TestIndexer_UnrelatedSuccessDoesNotMaskAFailedFile: a file whose re-index
// failed must not be forgotten when a later event for some OTHER file
// succeeds. That cycle proves the fault cleared but never revisits the failed
// file, so it must queue a full resync and keep the index failing until the
// resync succeeds, rather than report the index healthy while the failed
// file's change is missing. The retry timer is set far out, so it cannot be
// what recovers.
func TestIndexer_UnrelatedSuccessDoesNotMaskAFailedFile(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("a.go", "package p\n\nfunc A() {}\n")

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	idx.retryBase = time.Hour
	idx.Start()
	t.Cleanup(idx.Stop)
	if !waitFor(func() bool { h := idx.Health(); return h.State == "idle" && !h.LastSync.IsZero() }, 5*time.Second) {
		t.Fatalf("startup resync did not complete; health = %+v", idx.Health())
	}

	faults := []string{"inject_files_insert", "inject_files_update"}
	for _, stmt := range []string{
		`CREATE TRIGGER inject_files_insert BEFORE INSERT ON topology_files BEGIN SELECT RAISE(ABORT, '` + injectedIOErr + `'); END`,
		`CREATE TRIGGER inject_files_update BEFORE UPDATE ON topology_files BEGIN SELECT RAISE(ABORT, '` + injectedIOErr + `'); END`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("install fault trigger: %v", err)
		}
	}
	write("a.go", "package p\n\nfunc A() {}\n\nfunc A2() {}\n")
	idx.Enqueue("a.go", opUpsert)
	if !waitFor(func() bool { return idx.Health().Failing }, 5*time.Second) {
		t.Fatalf("a.go's re-index did not fail under the fault; health = %+v", idx.Health())
	}
	for _, name := range faults {
		if _, err := db.Exec(`DROP TRIGGER ` + name); err != nil {
			t.Fatalf("drop fault trigger: %v", err)
		}
	}

	write("b.go", "package p\n\nfunc B() {}\n")
	idx.Enqueue("b.go", opUpsert)
	if !waitFor(func() bool { h := idx.Health(); return !h.Failing && h.State == "idle" }, 5*time.Second) {
		t.Fatalf("index did not recover; health = %+v", idx.Health())
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM topology_nodes WHERE name = 'A2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("index reports healthy but a.go's A2 is not indexed (%d rows): the failed file was forgotten", n)
	}
}

// TestIndexer_OverflowResyncSettlesAFailure: the queue-overflow recovery
// resync a cycle runs is a full resync too, so after a failure it clears the
// failure itself rather than queueing a redundant catch-up resync. Runs the
// cycle synchronously, with no worker, so the queue can be inspected.
func TestIndexer_OverflowResyncSettlesAFailure(t *testing.T) {
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
	idx.reclaimFn = func() {}

	idx.setState("error", "boom")
	idx.resyncPending = true
	idx.runQueueCycle(indexOp{kind: opUpsert, path: "a.go"})

	if h := idx.Health(); h.Failing || h.State != "idle" {
		t.Fatalf("a cycle that completed an overflow resync left health %+v, want idle and not failing", h)
	}
	if n := len(idx.queue); n != 0 {
		t.Errorf("queued %d redundant catch-up op(s) after a cycle that already resynced", n)
	}
}

// TestScheduleRetry_OnlyAFailedRetryEscalates pins the backoff bookkeeping: a
// failing file-event cycle while a retry is pending leaves the pending retry's
// delay alone, only a failed retry (or catch-up) doubles it, a queued catch-up
// leaves it as it is, and only a healthy index resets it. The delays are
// hours, so the timer never fires here.
func TestScheduleRetry_OnlyAFailedRetryEscalates(t *testing.T) {
	idx := newIndexer(t.TempDir(), nil, nil, 0, 0)
	idx.retryBase, idx.retryMax = time.Hour, 4*time.Hour
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var r retryBackoff

	idx.setState("error", "boom")
	idx.scheduleRetry(timer, &r)
	if r != (retryBackoff{delay: time.Hour, armed: true}) {
		t.Fatalf("first failure: %+v, want the base delay armed", r)
	}
	idx.scheduleRetry(timer, &r) // a file-event cycle fails while the retry is pending
	if r != (retryBackoff{delay: time.Hour, armed: true}) {
		t.Fatalf("event failure with a retry pending escalated the backoff: %+v", r)
	}
	r.armed = false // the retry fired, as backgroundWorker records it
	idx.scheduleRetry(timer, &r)
	if r != (retryBackoff{delay: 2 * time.Hour, armed: true}) {
		t.Fatalf("failed retry: %+v, want the delay doubled and armed", r)
	}
	r.armed = false             // the retry fired, then a clean cycle queued a catch-up
	idx.setState("running", "") // a catch-up resync is queued; the index is still failing
	idx.scheduleRetry(timer, &r)
	if r != (retryBackoff{delay: 2 * time.Hour}) {
		t.Fatalf("a queued catch-up armed, reset or escalated the backoff: %+v", r)
	}
	idx.setState("error", "catch-up failed")
	idx.scheduleRetry(timer, &r)
	if r != (retryBackoff{delay: 4 * time.Hour, armed: true}) {
		t.Fatalf("failed catch-up: %+v, want the delay doubled and armed", r)
	}
	idx.setState("idle", "")
	idx.scheduleRetry(timer, &r)
	if r != (retryBackoff{}) {
		t.Fatalf("a healthy index left the backoff %+v, want it reset", r)
	}
}

// TestIndexer_CatchUpOncePerFailure: a clean cycle while failing queues ONE
// catch-up resync per failure. If that resync fails too, the fault is in the
// resync itself, and a full resync per clean file event would bypass the
// backoff: on an active workspace the worker would sit in back-to-back full
// walks. Later clean cycles leave the index in error for the retry timer, and
// going idle re-arms the catch-up for the next failure. Cycles run
// synchronously, with no worker, so the queue can be inspected.
func TestIndexer_CatchUpOncePerFailure(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		src := "package p\n\nfunc " + strings.ToUpper(strings.TrimSuffix(name, ".go")) + "() {}\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	idx.reclaimFn = func() {}
	queued := func() int {
		n := len(idx.queue)
		for len(idx.queue) > 0 {
			<-idx.queue // discard: this test plays the failed catch-up by hand
		}
		return n
	}

	idx.setState("error", "boom")
	idx.runQueueCycle(indexOp{kind: opUpsert, path: "a.go"})
	if n, h := queued(), idx.Health(); n != 1 || h.State != "running" || !h.Failing {
		t.Fatalf("first clean cycle while failing: queued %d, health %+v; want one catch-up, running and failing", n, h)
	}

	idx.setState("error", "catch-up failed") // the catch-up resync failed
	idx.runQueueCycle(indexOp{kind: opUpsert, path: "b.go"})
	if n, h := queued(), idx.Health(); n != 0 || h.State != "error" || !h.Failing {
		t.Fatalf("clean cycle after a failed catch-up: queued %d, health %+v; want nothing queued, left in error for the retry timer", n, h)
	}

	idx.runQueueCycle(indexOp{kind: opResync}) // a retry resync succeeds
	if h := idx.Health(); h.State != "idle" || h.Failing {
		t.Fatalf("successful resync: health %+v, want idle", h)
	}
	idx.setState("error", "a new failure")
	idx.runQueueCycle(indexOp{kind: opUpsert, path: "c.go"})
	if n := queued(); n != 1 {
		t.Fatalf("a new failure after recovery queued %d catch-ups, want 1: going idle must re-arm the catch-up", n)
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
