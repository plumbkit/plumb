package cli

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
)

func TestOnSessionIDStoresReplayedID(t *testing.T) {
	var s connSession
	s.onSessionID("sess-123")
	if got := s.view().replayedSessionID; got != "sess-123" {
		t.Fatalf("replayedSessionID = %q, want sess-123", got)
	}
	s.onSessionID("") // an empty id must no-op, not clear
	if got := s.view().replayedSessionID; got != "sess-123" {
		t.Fatalf("empty id cleared replayedSessionID, got %q", got)
	}
}

// TestOnSessionIDAdoptsReplayedID pins the PLAN-296 adoption half: a live
// connSession adopts the replayed stable ID as its own, so stats/memories/collab
// keyed on sessionID() see one continuous identity across the restart.
func TestOnSessionIDAdoptsReplayedID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	oldID := s.sessionID()
	if oldID == "" {
		t.Fatal("session did not register; the test would prove nothing")
	}

	s.onSessionID("replayed-id")
	if got := s.sessionID(); got != "replayed-id" {
		t.Fatalf("sessionID() = %q after adoption, want replayed-id", got)
	}
	if got := s.view().replayedSessionID; got != "replayed-id" {
		t.Fatalf("replayedSessionID = %q, want replayed-id", got)
	}
	// The old session file must be ended and the new one live.
	live, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := map[string]bool{}
	for _, info := range live {
		ids[info.ID] = true
	}
	if !ids["replayed-id"] {
		t.Fatal("the adopted ID is not live")
	}
	if ids[oldID] {
		t.Fatal("the pre-adoption ID is still live")
	}
}

// TestOnSessionIDDeclinesHeldID pins the overlap guard end to end: when the
// replayed ID is held by another live session, the connection keeps its
// generated ID rather than overwriting the holder.
func TestOnSessionIDDeclinesHeldID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	if _, err := session.Register(session.Info{ID: "held-id", Name: "holder"}); err != nil {
		t.Fatalf("Register holder: %v", err)
	}

	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	generated := s.sessionID()

	s.onSessionID("held-id")
	if got := s.sessionID(); got != generated {
		t.Fatalf("sessionID() = %q after a declined adoption, want the generated %q", got, generated)
	}
}
