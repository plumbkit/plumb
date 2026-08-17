package cli

import "testing"

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
