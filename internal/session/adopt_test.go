package session_test

import (
	"errors"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// TestAdoptMovesIdentity pins the PLAN-296 adoption path: re-registering under
// a new ID preserves the record (name included) and ends the old record, so a
// reconnecting connection keeps one continuous identity.
func TestAdoptMovesIdentity(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	oldReg, err := session.Register(session.Info{Name: "steady-heron"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	adopted, err := session.Adopt(oldReg.ID, "replayed-id")
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted.ID != "replayed-id" {
		t.Fatalf("adopted ID = %q, want replayed-id", adopted.ID)
	}
	if adopted.Name != "steady-heron" {
		t.Fatalf("adopted name = %q, want the preserved steady-heron", adopted.Name)
	}

	live, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := make(map[string]bool)
	for _, info := range live {
		ids[info.ID] = true
	}
	if !ids["replayed-id"] {
		t.Fatal("the adopted ID is not live after Adopt")
	}
	if ids[oldReg.ID] {
		t.Fatal("the old record is still live after Adopt; it should be ended")
	}
}

// TestAdoptCarriesPredecessorExternalID keeps the external session linkage that
// session_start wrote before the restart. The fresh adopter has none yet, so the
// adopted live record must take the predecessor's value without a second
// session_start call (PLAN-404).
func TestAdoptCarriesPredecessorExternalID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	predecessor, err := session.Register(session.Info{
		ID:         "predecessor-id",
		Name:       "steady-heron",
		ExternalID: "external-agent-id",
	})
	if err != nil {
		t.Fatalf("Register predecessor: %v", err)
	}
	session.Unregister(predecessor.ID)
	adopter, err := session.Register(session.Info{Name: "fresh-adopter"})
	if err != nil {
		t.Fatalf("Register adopter: %v", err)
	}

	adopted, err := session.Adopt(adopter.ID, predecessor.ID)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := adopted.ExternalID; got != predecessor.ExternalID {
		t.Fatalf("adopted ExternalID = %q, want predecessor %q", got, predecessor.ExternalID)
	}
	live, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, info := range live {
		if info.ID == predecessor.ID && info.ExternalID != predecessor.ExternalID {
			t.Fatalf("live ExternalID = %q, want %q", info.ExternalID, predecessor.ExternalID)
		}
	}
}

// TestAdoptDoesNotOverwriteAdopterExternalID pins the priority rule: a client
// that supplied an ExternalID before adoption keeps its own value rather than
// inheriting the predecessor's unrelated external identity.
func TestAdoptDoesNotOverwriteAdopterExternalID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	predecessor, err := session.Register(session.Info{
		ID:         "predecessor-id",
		Name:       "steady-heron",
		ExternalID: "predecessor-external-id",
	})
	if err != nil {
		t.Fatalf("Register predecessor: %v", err)
	}
	session.Unregister(predecessor.ID)
	adopter, err := session.Register(session.Info{
		Name:       "fresh-adopter",
		ExternalID: "adopter-external-id",
	})
	if err != nil {
		t.Fatalf("Register adopter: %v", err)
	}

	adopted, err := session.Adopt(adopter.ID, predecessor.ID)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := adopted.ExternalID; got != adopter.ExternalID {
		t.Fatalf("adopted ExternalID = %q, want adopter %q", got, adopter.ExternalID)
	}
}

// TestAdoptRefusesHeldID is the overlap guard: when another live session already
// holds the requested ID (the previous daemon still running), adoption declines
// with ErrIDTaken rather than overwriting that session's file.
func TestAdoptRefusesHeldID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if _, err := session.Register(session.Info{ID: "held-id", Name: "holder"}); err != nil {
		t.Fatalf("Register holder: %v", err)
	}
	adopter, err := session.Register(session.Info{Name: "adopter"})
	if err != nil {
		t.Fatalf("Register adopter: %v", err)
	}

	if _, err := session.Adopt(adopter.ID, "held-id"); !errors.Is(err, session.ErrIDTaken) {
		t.Fatalf("Adopt onto a held ID = %v, want ErrIDTaken", err)
	}
	// The adopter must remain live under its own ID — a declined adoption is a
	// no-op, not a partial re-register.
	live, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, info := range live {
		if info.ID == adopter.ID {
			return // still live: correct
		}
	}
	t.Fatal("the declined adopter vanished from the live session list")
}

// TestAdoptSameIDIsNoop pins that adopting the ID a session already holds is a
// no-op returning the existing record.
func TestAdoptSameIDIsNoop(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	reg, err := session.Register(session.Info{Name: "already-me"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := session.Adopt(reg.ID, reg.ID)
	if err != nil {
		t.Fatalf("Adopt same ID: %v", err)
	}
	if got.ID != reg.ID || got.Name != "already-me" {
		t.Fatalf("Adopt same ID returned %q/%q, want %q/already-me", got.ID, got.Name, reg.ID)
	}
}
