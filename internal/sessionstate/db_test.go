package sessionstate

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := openAt(filepath.Join(t.TempDir(), "session_state.db"))
	if err != nil {
		t.Fatalf("openAt: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestOpenStampsSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_state.db")
	s, err := openAt(path)
	if err != nil {
		t.Fatalf("openAt: %v", err)
	}
	defer s.Close()

	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", v, SchemaVersion)
	}
}

func TestUpsertLoadReadsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	// A non-round mtime to prove nanosecond fidelity survives the round-trip.
	mt := time.Unix(1_700_000_000, 123_456_789)

	if err := s.UpsertRead("proxyA", "/ws", "/ws/a.go", mt, "sha-a"); err != nil {
		t.Fatalf("UpsertRead: %v", err)
	}
	recs, err := s.LoadReads("proxyA", "/ws")
	if err != nil {
		t.Fatalf("LoadReads: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	got := recs[0]
	if got.Path != "/ws/a.go" || got.SHA != "sha-a" {
		t.Fatalf("record = %+v", got)
	}
	if !got.Mtime.Equal(mt) {
		t.Fatalf("mtime round-trip: got %v (nano %d), want %v (nano %d)",
			got.Mtime, got.Mtime.UnixNano(), mt, mt.UnixNano())
	}
}

func TestUpsertReadOverwrites(t *testing.T) {
	s := newTestStore(t)
	mt1 := time.Unix(1000, 0)
	mt2 := time.Unix(2000, 0)
	if err := s.UpsertRead("p", "/ws", "/ws/a.go", mt1, "old"); err != nil {
		t.Fatalf("UpsertRead 1: %v", err)
	}
	if err := s.UpsertRead("p", "/ws", "/ws/a.go", mt2, "new"); err != nil {
		t.Fatalf("UpsertRead 2: %v", err)
	}
	recs, err := s.LoadReads("p", "/ws")
	if err != nil {
		t.Fatalf("LoadReads: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (upsert, not insert)", len(recs))
	}
	if recs[0].SHA != "new" || !recs[0].Mtime.Equal(mt2) {
		t.Fatalf("record not overwritten: %+v", recs[0])
	}
}

func TestLoadReadsPerSessionIsolation(t *testing.T) {
	s := newTestStore(t)
	mt := time.Unix(1, 0)
	if err := s.UpsertRead("proxyA", "/ws", "/ws/a.go", mt, ""); err != nil {
		t.Fatalf("UpsertRead A: %v", err)
	}
	recs, err := s.LoadReads("proxyB", "/ws")
	if err != nil {
		t.Fatalf("LoadReads B: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("proxyB saw %d of proxyA's reads, want 0", len(recs))
	}
}

func TestLoadReadsWorkspaceScoping(t *testing.T) {
	s := newTestStore(t)
	mt := time.Unix(1, 0)
	if err := s.UpsertRead("p", "/ws1", "/ws1/a.go", mt, ""); err != nil {
		t.Fatalf("UpsertRead ws1: %v", err)
	}
	// Same proxy, different workspace must not leak — this is the invariant that
	// keeps a re-pin to a different project from resurrecting stale reads.
	recs, err := s.LoadReads("p", "/ws2")
	if err != nil {
		t.Fatalf("LoadReads ws2: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("workspace /ws2 saw %d of /ws1's reads, want 0", len(recs))
	}
}

func TestUpsertReadNoOpOnEmptyKey(t *testing.T) {
	s := newTestStore(t)
	cases := []struct{ id, ws, path string }{
		{"", "/ws", "/ws/a.go"},
		{"p", "", "/ws/a.go"},
		{"p", "/ws", ""},
	}
	for _, c := range cases {
		if err := s.UpsertRead(c.id, c.ws, c.path, time.Unix(1, 0), ""); err != nil {
			t.Fatalf("UpsertRead(%q,%q,%q): %v", c.id, c.ws, c.path, err)
		}
	}
	// Nothing should have been written.
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM read_tracking").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("rows written = %d, want 0 (empty-key upserts must be no-ops)", n)
	}
}

func TestPinRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertPin("p", "/ws", "go", PinSourceRoots); err != nil {
		t.Fatalf("UpsertPin: %v", err)
	}
	ws, lang, _, ok, err := s.LoadPin("p")
	if err != nil {
		t.Fatalf("LoadPin: %v", err)
	}
	if !ok || ws != "/ws" || lang != "go" {
		t.Fatalf("LoadPin = (%q,%q,%v), want (/ws, go, true)", ws, lang, ok)
	}
}

func TestPinUpsertOverwrites(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertPin("p", "/ws1", "go", PinSourceRoots); err != nil {
		t.Fatalf("UpsertPin 1: %v", err)
	}
	if err := s.UpsertPin("p", "/ws2", "rust", PinSourceSessionStart); err != nil {
		t.Fatalf("UpsertPin 2: %v", err)
	}
	ws, lang, _, ok, err := s.LoadPin("p")
	if err != nil {
		t.Fatalf("LoadPin: %v", err)
	}
	if !ok || ws != "/ws2" || lang != "rust" {
		t.Fatalf("LoadPin = (%q,%q,%v), want (/ws2, rust, true)", ws, lang, ok)
	}
}

func TestLoadPinMissing(t *testing.T) {
	s := newTestStore(t)
	_, _, _, ok, err := s.LoadPin("nope")
	if err != nil {
		t.Fatalf("LoadPin: %v", err)
	}
	if ok {
		t.Fatalf("LoadPin ok = true for an unrecorded proxy, want false")
	}
}

func TestPruneByTTL(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertRead("p", "/ws", "/ws/a.go", time.Unix(1, 0), ""); err != nil {
		t.Fatalf("UpsertRead: %v", err)
	}
	if err := s.UpsertPin("p", "/ws", "go", PinSourceRoots); err != nil {
		t.Fatalf("UpsertPin: %v", err)
	}
	// Backdate both rows so the prune cutoff (now) is strictly after them.
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	if _, err := s.db.Exec(`UPDATE read_tracking SET updated_at=?`, old); err != nil {
		t.Fatalf("backdate reads: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE pinned_workspace SET updated_at=?`, old); err != nil {
		t.Fatalf("backdate pin: %v", err)
	}

	if err := s.Prune(time.Now().Add(-24 * time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	recs, err := s.LoadReads("p", "/ws")
	if err != nil {
		t.Fatalf("LoadReads: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("reads survived prune: %d", len(recs))
	}
	if _, _, _, ok, _ := s.LoadPin("p"); ok {
		t.Fatalf("pin survived prune")
	}
}

func TestPruneKeepsFreshRows(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertRead("p", "/ws", "/ws/a.go", time.Unix(1, 0), ""); err != nil {
		t.Fatalf("UpsertRead: %v", err)
	}
	if err := s.Prune(time.Now().Add(-24 * time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	recs, err := s.LoadReads("p", "/ws")
	if err != nil {
		t.Fatalf("LoadReads: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("fresh read pruned: got %d, want 1", len(recs))
	}
}

func TestNilStoreMethodsAreSafe(t *testing.T) {
	var s *Store
	if err := s.UpsertRead("p", "/ws", "/ws/a.go", time.Unix(1, 0), ""); err != nil {
		t.Fatalf("nil UpsertRead: %v", err)
	}
	if recs, err := s.LoadReads("p", "/ws"); err != nil || recs != nil {
		t.Fatalf("nil LoadReads: %v %v", recs, err)
	}
	if err := s.UpsertPin("p", "/ws", "go", PinSourceRoots); err != nil {
		t.Fatalf("nil UpsertPin: %v", err)
	}
	if _, _, _, ok, err := s.LoadPin("p"); err != nil || ok {
		t.Fatalf("nil LoadPin: ok=%v err=%v", ok, err)
	}
	if err := s.Prune(time.Now()); err != nil {
		t.Fatalf("nil Prune: %v", err)
	}
	s.Close() // must not panic
}
