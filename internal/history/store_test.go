package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestEnqueueNeverBlocksAndOverflowKeepsTheChain(t *testing.T) {
	s := openTestStore(t, Options{QueueSize: 1, OverflowCap: 100, BatchWait: time.Hour})
	s.pauseWriter() // test hook: writer blocks until resumeWriter (see Step 3)
	p := filepath.Join(t.TempDir(), "f.txt")
	start := time.Now()
	for i := range 10 {
		s.Enqueue(item(OpUpdate, p, []byte{byte('a' + i), '\n'}, []byte{byte('b' + i), '\n'}))
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("10 Enqueues took %v with a full queue; Enqueue must not block", d)
	}
	s.resumeWriter()
	r := allRows(t, s)
	if len(r) != 10 {
		t.Fatalf("%d rows, want 10 (overflow must keep markers, not drop)", len(r))
	}
	overflow := 0
	for i, row := range r {
		if row.content == string(ContentOverflow) {
			overflow++
		}
		if i > 0 && row.seq < r[i-1].seq {
			t.Fatal("FIFO order lost across the channel/overflow boundary")
		}
	}
	if overflow == 0 {
		t.Fatal("positive control: with QueueSize 1 some rows must be overflow markers")
	}
}

func TestEnqueueDropsPastTheOverflowCapAndCountsIt(t *testing.T) {
	s := openTestStore(t, Options{QueueSize: 1, OverflowCap: 1, BatchWait: time.Hour})
	s.pauseWriter()
	p := filepath.Join(t.TempDir(), "f")
	for range 5 {
		s.Enqueue(item(OpUpdate, p, []byte("a\n"), []byte("b\n")))
	}
	s.resumeWriter()
	_ = allRows(t, s)
	var dropped string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key='dropped_rows'`).Scan(&dropped); err != nil || dropped == "0" {
		t.Fatalf("dropped_rows = %q, %v", dropped, err)
	}
}

func TestCloseWritesLeftoversAsMarkers(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "h.db"), Options{BatchWait: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s.pauseWriter()
	s.Enqueue(item(OpCreate, filepath.Join(t.TempDir(), "f"), nil, []byte("x\n")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	go func() { time.Sleep(20 * time.Millisecond); s.resumeWriter() }()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Reopen read-write to inspect.
	s2, err := Open(s.path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close(context.Background())
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM changes`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows after close = %d, %v; the leftover must be written (as a marker)", n, err)
	}
}

func TestInsertErrorDowngradesToAMarkerAndDoesNotWedge(t *testing.T) {
	s := openTestStore(t, Options{})
	bad := item(OpUpdate, filepath.Join(t.TempDir(), "f"), []byte("a\n"), []byte("b\n"))
	bad.Op = "bogus" // violates the op CHECK
	s.Enqueue(bad)
	s.Enqueue(item(OpUpdate, filepath.Join(t.TempDir(), "g"), []byte("a\n"), []byte("b\n")))
	r := allRows(t, s)
	if len(r) != 1 || r[0].path != "g" {
		t.Fatalf("rows = %+v; the poison item must not block the next one", r)
	}
	var errs string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key='write_errors'`).Scan(&errs); err != nil || errs == "0" {
		t.Fatalf("write_errors = %q, %v", errs, err)
	}
}
