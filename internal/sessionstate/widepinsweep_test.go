package sessionstate

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// wideUnder is a stand-in for internal/cli's containsUserHome. The real
// predicate probes the filesystem; this package must not care how the answer is
// reached, only that it is injected.
func wideUnder(root string) bool { return strings.HasPrefix(root, "/wide") }

// A pre-fix database can hold a wide root stamped session_start that was minted
// from the forged initialize _meta key (issue #318). Nothing in the row records
// which channel wrote it, and restoring one re-persists it, so the TTL prune
// never reaches it. The one-time sweep is what clears it — and it must spare
// every ordinary project pin.
func TestSweepWidePinsOnce_RemovesWideRowsAndSparesOthers(t *testing.T) {
	s := newTestStore(t)
	for _, p := range []struct {
		id, ws string
		src    PinSource
	}{
		{"forged", "/wide/Users", PinSourceSessionStart},
		{"declared", "/wide/home", PinSourceSessionStart},
		{"project", "/srv/proj", PinSourceSessionStart},
		{"rootsPin", "/srv/other", PinSourceRoots},
	} {
		if err := s.UpsertPin(p.id, p.ws, "go", p.src); err != nil {
			t.Fatalf("seed %s: %v", p.id, err)
		}
	}

	removed, err := s.SweepWidePinsOnce(wideUnder)
	if err != nil {
		t.Fatalf("SweepWidePinsOnce: %v", err)
	}
	slices.Sort(removed)
	if want := []string{"/wide/Users", "/wide/home"}; !slices.Equal(removed, want) {
		t.Fatalf("removed = %v, want %v", removed, want)
	}
	for _, id := range []string{"forged", "declared"} {
		if _, _, _, ok, err := s.LoadPin(id); err != nil || ok {
			t.Errorf("wide pin %q survived the sweep (ok=%v err=%v)", id, ok, err)
		}
	}
	// Ordinary projects are untouched whatever their origin: the sweep is about
	// the ROOT being wide, not about how it was pinned.
	for _, id := range []string{"project", "rootsPin"} {
		if _, _, _, ok, err := s.LoadPin(id); err != nil || !ok {
			t.Errorf("ordinary pin %q was swept (ok=%v err=%v)", id, ok, err)
		}
	}
}

// ONCE is the whole point. A wide root a human really did declare is
// indistinguishable from a forged one, so the sweep costs them a single
// re-declaration. Running on every start instead would make a deliberately
// declared wide workspace impossible to keep, which is issue #182's contract.
func TestSweepWidePinsOnce_DoesNotRunTwice(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertPin("p", "/wide/Users", "go", PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.SweepWidePinsOnce(wideUnder); err != nil || len(removed) != 1 {
		t.Fatalf("first sweep: removed=%v err=%v", removed, err)
	}

	// The caller re-declares the same wide root deliberately.
	if err := s.UpsertPin("p", "/wide/Users", "go", PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}
	removed, err := s.SweepWidePinsOnce(wideUnder)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("the sweep ran a second time and removed %v — a re-declared wide root must survive", removed)
	}
	if _, _, _, ok, err := s.LoadPin("p"); err != nil || !ok {
		t.Fatalf("the re-declared wide pin was swept again (ok=%v err=%v)", ok, err)
	}
}

// A database with NO wide pins must still disarm the sweep. Otherwise it stays
// armed on a clean install and fires on the first wide root the caller declares
// AFTER upgrading — a workspace this cleanup has no business touching, and a
// permanent breach of #182's "a declared wide root is yours to keep".
func TestSweepWidePinsOnce_DisarmsEvenWithNothingToRemove(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertPin("proj", "/srv/proj", "go", PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}

	if removed, err := s.SweepWidePinsOnce(wideUnder); err != nil || len(removed) != 0 {
		t.Fatalf("first sweep on a clean database: removed=%v err=%v", removed, err)
	}

	// The caller now deliberately declares a wide workspace.
	if err := s.UpsertPin("declared", "/wide/Users", "go", PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.SweepWidePinsOnce(wideUnder); err != nil || len(removed) != 0 {
		t.Fatalf("the sweep fired on a root declared AFTER it ran: removed=%v err=%v", removed, err)
	}
	if _, _, _, ok, err := s.LoadPin("declared"); err != nil || !ok {
		t.Fatalf("a wide root declared after the sweep was removed (ok=%v err=%v)", ok, err)
	}
}

// The swept workspace's read-tracking rows go with it — leaving them would keep
// a home directory's file list in the database after the pin is gone.
func TestSweepWidePinsOnce_DropsTheSweptWorkspacesReads(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertPin("forged", "/wide/Users", "go", PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRead("forged", "/wide/Users", "/wide/Users/me/.ssh/id_rsa", time.Unix(1, 0), "sha"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPin("proj", "/srv/proj", "go", PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRead("proj", "/srv/proj", "/srv/proj/main.go", time.Unix(1, 0), "sha"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SweepWidePinsOnce(wideUnder); err != nil {
		t.Fatalf("SweepWidePinsOnce: %v", err)
	}

	if reads, err := s.LoadReads("forged", "/wide/Users"); err != nil || len(reads) != 0 {
		t.Errorf("the swept workspace kept %d read row(s) (err=%v)", len(reads), err)
	}
	if reads, err := s.LoadReads("proj", "/srv/proj"); err != nil || len(reads) != 1 {
		t.Errorf("an untouched workspace lost its reads: %d (err=%v)", len(reads), err)
	}
}

// The flag lives in the database, not in memory, so a daemon restart does not
// re-run it. Same file, reopened.
func TestSweepWidePinsOnce_FlagSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_state.db")
	first, err := openAt(path)
	if err != nil {
		t.Fatalf("openAt: %v", err)
	}
	if err := first.UpsertPin("p", "/wide/Users", "go", PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}
	if removed, err := first.SweepWidePinsOnce(wideUnder); err != nil || len(removed) != 1 {
		t.Fatalf("first sweep: removed=%v err=%v", removed, err)
	}
	if err := first.UpsertPin("p", "/wide/Users", "go", PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := openAt(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	removed, err := second.SweepWidePinsOnce(wideUnder)
	if err != nil {
		t.Fatalf("sweep after reopen: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("the sweep re-ran after a reopen and removed %v — the flag must be persisted, not in-memory", removed)
	}
}

// Nil-safe in both directions: a daemon whose session-state store failed to open
// passes a nil Store, and a caller with no predicate must not sweep blindly.
func TestSweepWidePinsOnce_NilSafe(t *testing.T) {
	var nilStore *Store
	if removed, err := nilStore.SweepWidePinsOnce(wideUnder); err != nil || removed != nil {
		t.Errorf("nil store: removed=%v err=%v", removed, err)
	}
	s := newTestStore(t)
	if err := s.UpsertPin("p", "/wide/Users", "go", PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.SweepWidePinsOnce(nil); err != nil || removed != nil {
		t.Errorf("nil predicate: removed=%v err=%v", removed, err)
	}
	if _, _, _, ok, _ := s.LoadPin("p"); !ok {
		t.Error("a nil predicate swept a pin — with no way to answer 'is this wide?' it must do nothing")
	}
}
