package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// TestLeaveNote_ThreadReplyBindsToThePeersIDAcrossARename is the reply half of
// the impersonation fix. A thread records the peer's session ID, and the reply
// must bind to THAT ID rather than to whatever a name re-resolution returns: a
// peer that renamed since its last note no longer resolves by the recorded
// name, and trusting the empty resolution would write the reply UNBOUND, where
// the next holder of the name could claim it.
func TestLeaveNote_ThreadReplyBindsToThePeersIDAcrossARename(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	ctx := context.Background()

	// A thread whose only other participant is a peer that has since renamed.
	// Its ID is in the rows even though its recorded name no longer resolves.
	conv, err := store.PutNote(ctx, collab.NoteInput{
		AuthorSession: "alice",
		AuthorID:      "sess-alice-1",
		Body:          "question",
		Addressee:     deps.SessionName(),
		TTL:           time.Hour,
	}, time.Now())
	if err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	// The peer renamed, so resolving the recorded name finds no live session.
	deps.ResolvePeer = func(string) (PeerSession, bool) { return PeerSession{}, false }

	out, err := NewLeaveNote(deps).Execute(ctx,
		json.RawMessage(`{"conversation_id":`+jsonStr(conv)+`,"body":"answer"}`))
	if err != nil {
		t.Fatalf("a thread reply should not error: %v", err)
	}
	if strings.Contains(out, "Not sent") {
		t.Fatalf("the reply was refused: %q", out)
	}

	// The reply must be claimable by the peer's session ID, and only by it.
	bound, err := store.PendingNotes(ctx, collab.Claimant{Name: "alice", ID: "sess-alice-1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(bound) != 1 {
		t.Fatalf("pending for the peer's ID = %d, want the one bound reply", len(bound))
	}
	if got := bound[0].AddresseeID; got != "sess-alice-1" {
		t.Fatalf("stored AddresseeID = %q, want the thread's sess-alice-1; an empty binding is claimable by a name reuser", got)
	}
	unbound, err := store.PendingNotes(ctx, collab.Claimant{Name: "alice", ID: "sess-imposter"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(unbound) != 0 {
		t.Fatalf("a different session holding the name saw %d bound message(s)", len(unbound))
	}
}
