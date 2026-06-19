package topology

import (
	"os"
	"path/filepath"
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
