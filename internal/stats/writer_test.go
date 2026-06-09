package stats

import (
	"testing"
	"time"
)

func sampleCall(session, tool string) Call {
	return Call{
		SessionID: session,
		Workspace: "/w",
		Tool:      tool,
		CalledAt:  time.Now(),
		Success:   true,
	}
}

func countRows(t *testing.T, sessionID string) int64 {
	t.Helper()
	db, err := OpenReadOnly()
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if db == nil {
		return 0
	}
	defer db.Close()
	summary, err := db.Summary(Filter{SessionID: sessionID})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	var n int64
	for _, row := range summary {
		n += row.Calls
	}
	return n
}

// TestWriter_DrainsOnClose verifies Close flushes every buffered insert before
// returning — a clean shutdown must not lose stats.
func TestWriter_DrainsOnClose(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	w, err := NewWriter()
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// More than one batch worth, to exercise the batching boundary.
	const n = writerMaxBatch + 37
	for range n {
		w.Record(sampleCall("drain", "read_file"))
	}
	w.Close()

	if got := countRows(t, "drain"); got != n {
		t.Fatalf("after Close: %d rows; want %d", got, n)
	}
	if d := w.Dropped(); d != 0 {
		t.Fatalf("dropped %d calls under buffer capacity; want 0", d)
	}
}

// TestWriter_DropsOnOverflow verifies a full buffer drops the overflow and
// counts it, rather than blocking the caller. The writer goroutine is not
// started, so nothing drains the channel — the drop is deterministic.
func TestWriter_DropsOnOverflow(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	const buf = 4
	const sent = 10
	w := newWriter(db, buf) // run() intentionally not started
	for range sent {
		w.Record(sampleCall("overflow", "read_file"))
	}
	if got, want := w.Dropped(), int64(sent-buf); got != want {
		t.Fatalf("dropped %d; want %d (buffer %d, sent %d)", got, want, buf, sent)
	}
}

// TestWriter_RenameAppliesAfterInsert verifies RenameSession flows through the
// same goroutine and names rows that were already inserted (ordering: the
// pending batch is flushed before the rename runs).
func TestWriter_RenameAppliesAfterInsert(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	w, err := NewWriter()
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.Record(sampleCall("rename-me", "edit_file"))
	w.RenameSession("rename-me", "swift-falcon")
	w.Close()

	ro, err := OpenReadOnly()
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	var name string
	// writer_test.go is in package stats, so the unexported handle is reachable.
	if err := ro.db.QueryRow(
		`SELECT session_name FROM tool_calls WHERE session_id = ?`, "rename-me",
	).Scan(&name); err != nil {
		t.Fatalf("query session_name: %v", err)
	}
	if name != "swift-falcon" {
		t.Fatalf("session_name = %q; want %q (rename did not apply after insert)", name, "swift-falcon")
	}
}

// TestRecordBatch_SkipsInvalid verifies a batch inserts the valid rows, skips
// (and counts) rows missing required fields, and commits as a whole.
func TestRecordBatch_SkipsInvalid(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	batch := []Call{
		sampleCall("batch", "read_file"),
		{SessionID: "batch", Workspace: "/w", CalledAt: time.Now()}, // missing tool
		sampleCall("batch", "edit_file"),
	}
	skipped, err := db.RecordBatch(batch)
	if err != nil {
		t.Fatalf("RecordBatch: %v", err)
	}
	if skipped != 1 {
		t.Fatalf("skipped %d; want 1", skipped)
	}
	if got := countRows(t, "batch"); got != 2 {
		t.Fatalf("inserted %d valid rows; want 2", got)
	}
}
