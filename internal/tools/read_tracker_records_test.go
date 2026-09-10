package tools

import (
	"testing"
	"time"
)

// TestReadTracker_RecordsRoundTrip: a snapshot hydrates an empty tracker into
// the same observed state, and a nil tracker snapshots to nothing.
func TestReadTracker_RecordsRoundTrip(t *testing.T) {
	src := NewReadTracker()
	when := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	src.Record("/w/a.go", when, "sha-a")
	src.Record("/w/sub/../b.go", when.Add(time.Second), "sha-b")

	recs := src.Records()
	if len(recs) != 2 {
		t.Fatalf("Records() = %d entries, want 2", len(recs))
	}
	dst := NewReadTracker()
	dst.Hydrate(recs)
	if got := dst.Mtime("/w/a.go"); !got.Equal(when) {
		t.Fatalf("a.go mtime = %v, want %v", got, when)
	}
	if got := dst.Mtime("/w/b.go"); !got.Equal(when.Add(time.Second)) {
		t.Fatalf("b.go mtime = %v, want %v (cleaned path must survive)", got, when.Add(time.Second))
	}
	var nilTracker *ReadTracker
	if got := nilTracker.Records(); got != nil {
		t.Fatalf("nil tracker must snapshot to nil, got %v", got)
	}
}
