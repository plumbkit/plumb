package memory

import (
	"errors"
	"testing"
	"time"
)

// TestIndex_CloseJoinsTheBackgroundReindex (PLAN-535): Close does not return
// while a background reindex it started is still running, and none starts after.
func TestIndex_CloseJoinsTheBackgroundReindex(t *testing.T) {
	ix, err := OpenIndex(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	finished := make(chan struct{})
	if !ix.goBackground(func() { <-release; close(finished) }) {
		t.Fatal("control: a background task did not start on an open index")
	}
	closed := make(chan struct{})
	go func() { _ = ix.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a background reindex was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the background reindex ended")
	}
	select {
	case <-finished:
	default:
		t.Fatal("Close returned before the background task finished")
	}
	if ix.goBackground(func() {}) {
		t.Fatal("a background task started on a closed index")
	}
}

// TestIndex_ReindexStopsWhenClosing: a Reindex already past its first memory
// stops at the next one once Close has begun.
func TestIndex_ReindexStopsWhenClosing(t *testing.T) {
	ws := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := Write(ws, name, "body "+name, "d"); err != nil {
			t.Fatal(err)
		}
	}
	ix, err := OpenIndex(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ix.stop.Store(true)
	if _, err := ix.Reindex(ws); !errors.Is(err, ErrIndexClosing) {
		t.Fatalf("Reindex on a closing index = %v, want ErrIndexClosing", err)
	}
}

// TestIndex_ReindexAfterCloseReportsClosing: Close itself raises the stop flag,
// so a Reindex after it reports ErrIndexClosing.
func TestIndex_ReindexAfterCloseReportsClosing(t *testing.T) {
	ws := t.TempDir()
	ix, err := OpenIndex(ws)
	if err != nil {
		t.Fatal(err)
	}
	_ = ix.Close()
	if _, err := ix.Reindex(ws); !errors.Is(err, ErrIndexClosing) {
		t.Fatalf("Reindex after Close = %v, want ErrIndexClosing", err)
	}
}
