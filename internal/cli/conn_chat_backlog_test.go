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
// preview: a batch that fills the per-call cap must SAY so, because "3 waiting"
// alone reads as "all of them" and an agent that goes idle here never learns a
// backlog is still queued. The next call shows the remainder, and that one —
// uncapped — must not claim a backlog.
func TestMessageHint_BacklogNamesRemainder(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	for range 4 {
		seedMessage(t, s, ws, "bob", "alice", "burst")
	}

	got := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "On branch main\n")
	if !strings.Contains(got, "3 waiting") {
		t.Fatalf("first preview should carry the capped batch of 3; got:\n%s", got)
	}
	if !strings.Contains(got, "1 more waiting as of this call") {
		t.Errorf("a capped delivery must name the remainder; got:\n%s", got)
	}

	got2 := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "On branch main\n")
	if !strings.Contains(got2, "burst") {
		t.Errorf("the invalidated cache must let the next call show the remainder; got:\n%s", got2)
	}
	if strings.Contains(got2, "more waiting") {
		t.Errorf("the uncapped remainder must not claim a backlog; got:\n%s", got2)
	}
	// The previews consumed nothing, so all four are still there for the channel
	// that actually delivers. Two claims because the claim keeps its own cap.
	claimed := len(s.inbox().Claim(context.Background())) + len(s.inbox().Claim(context.Background()))
	if claimed != 4 {
		t.Errorf("previewing must leave every note claimable; check_messages got %d of 4", claimed)
	}
}

// TestMessageHint_KeepsDeliveredNotes wires the recipient policy end to end, and
// pins which event triggers permanence now that the piggyback claims nothing.
//
// keep_delivered_notes makes DELIVERY the permanence trigger, and a preview is
// not a delivery — so a previewed note keeps the TTL it was sent with, exactly
// like any other unread note, and only the claim stamps it far-future. That is
// the documented meaning of the TTL (it bounds a note while it is UNREAD) rather
// than a concession: a note nobody has read should not become a permanent
// transcript entry on the strength of a block that may have gone nowhere.
func TestMessageHint_KeepsDeliveredNotes(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{
		Mailbox: true, ChatBudgetBytes: 512, KeepDeliveredNotes: true,
	})
	seedMessage(t, s, ws, "bob", "alice", "kept by the claim")
	farFuture := time.Now().Add(200 * 365 * 24 * time.Hour)

	if got := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "out\n"); !strings.Contains(got, "kept by the claim") {
		t.Fatalf("message must be previewed; got %q", got)
	}
	store := s.collabPool.acquire(ws)
	rows, err := store.SentBy(context.Background(), "peer", time.Now(), 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("SentBy = %d rows (err %v), want the sent row", len(rows), err)
	}
	if rows[0].ExpiresAt.After(farFuture) {
		t.Error("a preview must not stamp permanence — the note has not been read yet")
	}

	// The claim check_messages makes is the trigger.
	if claimed := s.inbox().Claim(context.Background()); len(claimed) != 1 {
		t.Fatalf("the previewed note must still be claimable; claimed %d", len(claimed))
	}
	rows, err = store.SentBy(context.Background(), "peer", time.Now(), 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("SentBy = %d rows (err %v), want the kept transcript row", len(rows), err)
	}
	if !rows[0].ExpiresAt.After(farFuture) {
		t.Error("under keep_delivered_notes the claim must stamp the row far-future")
	}
}
