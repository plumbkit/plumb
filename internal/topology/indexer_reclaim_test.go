package topology

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestIndexer_IdleReclaim_TrickleDrainsArenaPool is the regression guard for the
// daemon memory-retention bug: gotreesitter's parse-arena pool is a package-global
// strong-reference free-list, so a parsed file's high-water-mark arena stays
// resident until the pool is explicitly drained. The only steady-state drain used
// to be the per-cycle burst gate (shouldReclaimAfterBurst, >= 64 files in one
// cycle); a realistic workload of single-file edits never tripped it, and with a
// live file watcher the periodic resync is suppressed, so ~150 MB of pooled arenas
// sat resident on an otherwise idle daemon. The idle-reclaim timer must drain the
// pool once the queue goes quiet, even for a trickle that never bursts.
func TestIndexer_IdleReclaim_TrickleDrainsArenaPool(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// One file so the startup resync has something to index.
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatalf("write a.go: %v", err)
	}

	reclaims := make(chan struct{}, 64)
	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	idx.idleReclaim = 30 * time.Millisecond
	idx.reclaimFn = func() { reclaims <- struct{}{} }
	idx.Start()
	t.Cleanup(idx.Stop)

	// The startup resync reclaims once; wait for it so we can isolate the
	// trickle-driven idle reclaim that follows.
	select {
	case <-reclaims:
	case <-time.After(5 * time.Second):
		t.Fatal("startup resync did not reclaim the arena pool")
	}

	// A single-file edit is a trickle: it never trips the per-cycle burst gate,
	// so before this fix the pooled arenas would sit resident indefinitely.
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("package p\n\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatalf("write b.go: %v", err)
	}
	idx.Enqueue("b.go", opUpsert)

	select {
	case <-reclaims:
		// Success: the trickle's idle window fired a reclaim.
	case <-time.After(5 * time.Second):
		t.Fatal("idle-reclaim did not drain the arena pool after a single-file edit")
	}
}

// TestIndexer_IdleReclaim_DisabledByZero confirms idleReclaim == 0 turns the idle
// drain off entirely (so the hot path pays no extra GC), and that a single edit
// does NOT reclaim on its own without the timer — i.e. the trickle reclaim is
// genuinely driven by the idle window, not by every cycle.
func TestIndexer_IdleReclaim_DisabledByZero(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatalf("write a.go: %v", err)
	}

	reclaims := make(chan struct{}, 64)
	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	idx.idleReclaim = 0 // disabled
	idx.reclaimFn = func() { reclaims <- struct{}{} }
	idx.Start()
	t.Cleanup(idx.Stop)

	// Drain the one-off startup resync reclaim (resync always drains its own
	// transient working set regardless of the idle timer).
	select {
	case <-reclaims:
	case <-time.After(5 * time.Second):
		t.Fatal("startup resync did not reclaim the arena pool")
	}

	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("package p\n\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatalf("write b.go: %v", err)
	}
	idx.Enqueue("b.go", opUpsert)

	select {
	case <-reclaims:
		t.Fatal("idle-reclaim fired with idleReclaim=0: a trickle must not drain the pool when disabled")
	case <-time.After(250 * time.Millisecond):
		// Success: no reclaim while disabled.
	}
}

// TestRunQueueCycle_ReclaimedReturn locks the bool contract that drives the
// idle-drain cancellation in backgroundWorker: runQueueCycle reports true only
// when it actually reclaimed the arena pool — a burst, or a *successful* resync
// (both drain) — and false otherwise. The failed-resync case is the key one: a
// resync that errors at the walk/prune step never reached processResync's final
// drain, so it must NOT credit a reclaim, or the worker would cancel the
// idle-reclaim backstop without anything having been freed.
func TestRunQueueCycle_ReclaimedReturn(t *testing.T) {
	t.Run("trickle returns false and does not reclaim", func(t *testing.T) {
		dir := t.TempDir()
		db := mustOpenDB(t, dir)
		var n int
		idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
		idx.reclaimFn = func() { n++ }
		if got := idx.runQueueCycle(indexOp{kind: opUpsert, path: "x.go"}); got {
			t.Errorf("trickle runQueueCycle = true, want false")
		}
		if n != 0 {
			t.Errorf("reclaimFn called %d times on a trickle, want 0", n)
		}
	})

	t.Run("burst returns true and reclaims once", func(t *testing.T) {
		dir := t.TempDir()
		db := mustOpenDB(t, dir)
		var n int
		idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
		idx.reclaimFn = func() { n++ }
		for i := 0; i < reclaimAfterOps; i++ {
			idx.queue <- indexOp{kind: opUpsert, path: fmt.Sprintf("f%d.go", i)}
		}
		if got := idx.runQueueCycle(indexOp{kind: opUpsert, path: "seed.go"}); !got {
			t.Errorf("burst runQueueCycle = false, want true")
		}
		if n != 1 {
			t.Errorf("reclaimFn called %d times on a burst, want 1", n)
		}
	})

	t.Run("successful resync returns true and reclaims", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		db := mustOpenDB(t, dir)
		var n int
		idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
		idx.reclaimFn = func() { n++ }
		if got := idx.runQueueCycle(indexOp{kind: opResync}); !got {
			t.Errorf("successful resync runQueueCycle = false, want true")
		}
		if n != 1 {
			t.Errorf("reclaimFn called %d times on a successful resync, want 1", n)
		}
	})

	t.Run("failed resync returns false and does not reclaim", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		db := mustOpenDB(t, dir)
		db.Close() // every DB op in the resync now errors → processResync fails before its drain
		var n int
		idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
		idx.reclaimFn = func() { n++ }
		if got := idx.runQueueCycle(indexOp{kind: opResync}); got {
			t.Errorf("failed resync runQueueCycle = true, want false (no drain ran; the idle backstop must stay armed)")
		}
		if n != 0 {
			t.Errorf("reclaimFn called %d times on a failed resync, want 0", n)
		}
	})
}

// gatedExtractor blocks its first Extract until release is closed, letting a
// test hold one queue cycle open while it enqueues the next so the two cannot
// coalesce.
type gatedExtractor struct {
	release  chan struct{}
	gateOnce sync.Once
}

func (g *gatedExtractor) Language() string     { return "go" }
func (g *gatedExtractor) Extensions() []string { return []string{".go"} }
func (g *gatedExtractor) Extract(_ context.Context, _ string, _ []byte) ([]Node, []Edge, error) {
	g.gateOnce.Do(func() { <-g.release })
	return nil, nil, nil
}

// TestIndexer_BurstReclaim_CancelsPendingIdleDrain exercises the worker branch
// that is the whole reason runQueueCycle returns bool: a trickle arms the
// idle-reclaim timer, and a burst that follows before it fires must cancel it
// (the burst gate already drained, so the pending idle drain would be redundant
// — and, more importantly, the bookkeeping must stay correct). The gated
// extractor makes the ordering deterministic: the trickle cycle is held open in
// Extract while the burst is enqueued, so the burst is guaranteed to land in a
// single following cycle.
func TestIndexer_BurstReclaim_CancelsPendingIdleDrain(t *testing.T) {
	dir := t.TempDir()
	db := mustOpenDB(t, dir)
	t.Cleanup(func() { db.Close() })

	var reclaims atomic.Int64
	gate := &gatedExtractor{release: make(chan struct{})}
	idx := newIndexer(dir, db, []Extractor{gate}, 512*1024, 0)
	idx.idleReclaim = 200 * time.Millisecond
	idx.reclaimFn = func() { reclaims.Add(1) }
	idx.Start()
	t.Cleanup(idx.Stop)

	// Empty workspace: the startup resync finds nothing to extract (so it never
	// hits the gate), drains once, and goes quiet. Wait for that reclaim.
	waitForReclaims(t, &reclaims, 1)

	// A single-file edit is a trickle that arms the idle timer; its extract blocks
	// on the gate, holding the cycle open.
	if err := os.WriteFile(filepath.Join(dir, "trickle.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx.Enqueue("trickle.go", opUpsert)
	time.Sleep(20 * time.Millisecond) // let the worker reach the gated Extract

	// While the trickle cycle is parked, enqueue a burst. It buffers and cannot be
	// drained until the trickle cycle finishes, so it coalesces into one following
	// cycle that trips the burst gate.
	for i := 0; i <= reclaimAfterOps; i++ {
		idx.Enqueue(fmt.Sprintf("b%d.go", i), opUpsert)
	}
	close(gate.release) // trickle cycle completes (arms timer); burst cycle follows and cancels it

	// Wait well past idleReclaim. A correct burst cancels the trickle's pending
	// idle drain, so the only reclaims are startup(1) + burst(1) = 2. A third would
	// mean the cancelled timer still fired.
	time.Sleep(500 * time.Millisecond)
	if got := reclaims.Load(); got != 2 {
		t.Fatalf("reclaims = %d, want 2 (startup resync + burst gate); a 3rd means the burst did not cancel the trickle's pending idle drain", got)
	}
}

func mustOpenDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(dir, ".plumb", "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	return db
}

func waitForReclaims(t *testing.T, n *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d reclaim(s), got %d", want, n.Load())
}
