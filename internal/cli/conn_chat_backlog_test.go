package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// TestMessageHint_BacklogNamesRemainder is the legibility half of the capped
// delivery: a batch that fills the per-call cap must SAY so, because "3 new"
// alone reads as "all of them" and an agent that goes idle here never learns a
// backlog is still queued. The next call drains the remainder, and that
// delivery — uncapped — must not claim a backlog.
func TestMessageHint_BacklogNamesRemainder(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	for range 4 {
		seedMessage(t, s, ws, "bob", "alice", "burst")
	}

	got := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "On branch main\n")
	if !strings.Contains(got, "3 new") {
		t.Fatalf("first delivery should carry the capped batch of 3; got:\n%s", got)
	}
	if !strings.Contains(got, "1 more waiting as of this call") {
		t.Errorf("a capped delivery must name the remainder; got:\n%s", got)
	}

	got2 := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "On branch main\n")
	if !strings.Contains(got2, "burst") {
		t.Errorf("the invalidated cache must let the next call drain the remainder; got:\n%s", got2)
	}
	if strings.Contains(got2, "more waiting") {
		t.Errorf("the uncapped drain must not claim a backlog; got:\n%s", got2)
	}
}

// TestMessageHint_KeepsDeliveredNotes wires the recipient policy end to end:
// the piggyback claim — not just check_messages — is the permanence trigger,
// because every delivery path shares Inbox.Claim.
func TestMessageHint_KeepsDeliveredNotes(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{
		Mailbox: true, ChatBudgetBytes: 512, KeepDeliveredNotes: true,
	})
	seedMessage(t, s, ws, "bob", "alice", "kept by the claim")

	if got := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "out\n"); !strings.Contains(got, "kept by the claim") {
		t.Fatalf("message must deliver; got %q", got)
	}
	store := s.collabPool.acquire(ws)
	rows, err := store.SentBy(context.Background(), "peer", time.Now(), 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("SentBy = %d rows (err %v), want the kept transcript row", len(rows), err)
	}
	if !rows[0].ExpiresAt.After(time.Now().Add(200 * 365 * 24 * time.Hour)) {
		t.Error("under keep_delivered_notes the piggyback claim must stamp the row far-future")
	}
}
