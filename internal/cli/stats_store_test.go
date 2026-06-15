package cli

import (
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/stats"
)

// TestStatsStore_RecordReturnsFast verifies Record dispatches the write
// asynchronously — the MCP response path must not block on stats SQLite.
func TestStatsStore_RecordReturnsFast(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newStatsStore()
	defer s.Close()

	// Warm the lazily-opened writer first: the one-time DB open + migration is a
	// synchronous cost on the first Record that has nothing to do with whether
	// the enqueue path blocks — and on a loaded CI runner it alone can exceed the
	// budget. Timing only the steady-state enqueues keeps the regression intent
	// (no SQLite write on the response path) without the cold-start flakiness.
	s.ensureWriter()

	const n = 50
	start := time.Now()
	for range n {
		s.Record("/w", stats.Call{
			SessionID: "sess-fast",
			Tool:      "read_file",
			CalledAt:  time.Now(),
			Success:   true,
		})
	}
	elapsed := time.Since(start)

	// Fifty enqueues should be tens of microseconds, certainly well under the
	// per-call BUSY-timeout floor (5 s). 200 ms is generous slack for CI noise
	// while still catching a regression that put the DB write on this path.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Record blocked: %d enqueues took %s; want < 200ms", n, elapsed)
	}
}

// TestStatsStore_CloseDrainsInFlight verifies Close blocks until in-flight
// async writes finish — so a clean daemon shutdown does not drop stats rows.
func TestStatsStore_CloseDrainsInFlight(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newStatsStore()

	const n = 25
	for range n {
		s.Record("/w", stats.Call{
			SessionID: "sess-drain",
			Tool:      "edit_file",
			CalledAt:  time.Now(),
			Success:   true,
		})
	}
	s.Close()

	// Reopen the underlying DB and count rows — every Record call must have
	// landed before Close returned.
	db, err := stats.OpenReadOnly()
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()
	summary, err := db.Summary(stats.Filter{SessionID: "sess-drain"})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	var got int64
	for _, row := range summary {
		got += row.Calls
	}
	if got != n {
		t.Fatalf("recorded %d rows after Close; want %d (Close did not drain in-flight writes)", got, n)
	}
}

// TestStatsStore_RecordAfterCloseDropped verifies that a Record racing past
// Close is silently dropped rather than panicking on a closed DB.
func TestStatsStore_RecordAfterCloseDropped(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newStatsStore()
	s.Close()

	// Must not panic.
	s.Record("/w", stats.Call{
		SessionID: "post-close",
		Tool:      "read_file",
		CalledAt:  time.Now(),
		Success:   true,
	})
}

// TestStatsStore_ConcurrentRecord stresses the lock-protected closing flag
// against concurrent Records — no double-close, no spawned goroutine after
// Close marks the store closing.
func TestStatsStore_ConcurrentRecord(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newStatsStore()

	var wg sync.WaitGroup
	const writers = 8
	const each = 20
	for range writers {
		wg.Go(func() {
			for range each {
				s.Record("/w", stats.Call{
					SessionID: "sess-conc",
					Tool:      "read_file",
					CalledAt:  time.Now(),
					Success:   true,
				})
			}
		})
	}
	wg.Wait()
	s.Close()

	db, err := stats.OpenReadOnly()
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()
	summary, err := db.Summary(stats.Filter{SessionID: "sess-conc"})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	var got int64
	for _, row := range summary {
		got += row.Calls
	}
	if want := int64(writers * each); got != want {
		t.Fatalf("recorded %d rows after concurrent Record; want %d", got, want)
	}
}
