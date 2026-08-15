package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// leave_note_thread_test.go pins in-thread reply addressing.
//
// Quoting a conversation_id is an unambiguous statement of intent to reply in
// that thread. Defaulting the addressee to "next" contradicted it, and did so
// silently: the reply went to whoever attached next rather than the peer being
// answered, and since "next" matches any claimant, the author's own session was
// frequently the one that took it. The receipt still said the note was sent.

// seedThread puts one note from peer to self, creating a thread self can reply
// into. Returns the conversation id.
func seedThread(t *testing.T, store *collab.Store, peer, self string) string {
	t.Helper()
	conv, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: peer, AuthorID: "sess-" + peer,
		Body: "opening question", Addressee: self, TTL: time.Hour,
	}, time.Now())
	if err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	return conv
}

// TestLeaveNote_InThreadReplyReachesTheOtherParticipant is the fix: no `to`, but
// a conversation_id, resolves to the peer on the other side of that thread.
func TestLeaveNote_InThreadReplyReachesTheOtherParticipant(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	conv := seedThread(t, store, "alice", "test-session")

	tool := NewLeaveNote(deps)
	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"body":"answering you","conversation_id":`+jsonStr(conv)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "next session") {
		t.Errorf("in-thread reply was re-addressed to the next arrival: %q", out)
	}

	pending, err := store.PendingNotes(context.Background(), collab.Claimant{Name: "alice"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range pending {
		if strings.Contains(r.Body, "answering you") {
			found = true
			if r.Addressee != "alice" {
				t.Errorf("addressee = %q, want alice", r.Addressee)
			}
		}
	}
	if !found {
		t.Fatalf("alice never received the reply; pending = %d", len(pending))
	}
}

// TestLeaveNote_InThreadReplyIsNeverClaimedByItsAuthor is the end-to-end
// statement of the bug: send a reply with no `to`, then have the AUTHOR check
// its own mail. Before the fix it was handed its own message and, because
// delivery is exactly-once, the peer could then never receive it.
func TestLeaveNote_InThreadReplyIsNeverClaimedByItsAuthor(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	conv := seedThread(t, store, "alice", "test-session")

	tool := NewLeaveNote(deps)
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"body":"my reply","conversation_id":`+jsonStr(conv)+`}`)); err != nil {
		t.Fatal(err)
	}

	self := collab.Claimant{Name: "test-session", ID: "sess-1"}
	got, err := store.ClaimNotes(context.Background(), self, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if strings.Contains(r.Body, "my reply") {
			t.Fatal("the author claimed its own reply, consuming the peer's only copy")
		}
	}

	// And the peer can still collect it afterwards.
	pending, err := store.PendingNotes(context.Background(), collab.Claimant{Name: "alice"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var delivered bool
	for _, r := range pending {
		delivered = delivered || strings.Contains(r.Body, "my reply")
	}
	if !delivered {
		t.Fatal("the reply is gone: neither the author nor the peer holds it")
	}
}

// TestLeaveNote_InThreadReplyRefusesWhenTheThreadIsUnknown: a conversation with
// no other participant on record cannot be replied to, so the caller is told,
// rather than having the note quietly filed for the next arrival.
func TestLeaveNote_InThreadReplyRefusesWhenTheThreadIsUnknown(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})

	tool := NewLeaveNote(deps)
	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"body":"into the void","conversation_id":"no-such-thread"}`))
	if err != nil {
		t.Fatalf("a refusal must not be an error: %v", err)
	}
	if !strings.Contains(out, "Not sent") || !strings.Contains(out, "no other participant") {
		t.Errorf("expected an explicit refusal naming the problem; got %q", out)
	}
	if !strings.Contains(out, "`to`") {
		t.Errorf("the refusal must name the remedy; got %q", out)
	}

	// Nothing was stored: a refused send consumes nothing.
	pending, err := store.PendingNotes(context.Background(), collab.Claimant{Name: "anyone"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range pending {
		if strings.Contains(r.Body, "into the void") {
			t.Fatal("a refused send still wrote a row")
		}
	}
}

// TestLeaveNote_InThreadReplyRefusesWhenAmbiguous: with more than one other
// participant, "the other one" has no answer, so the tool names them and asks.
func TestLeaveNote_InThreadReplyRefusesWhenAmbiguous(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	conv := seedThread(t, store, "alice", "test-session")
	// A third party joins the same thread.
	if _, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob",
		Body: "me too", Addressee: "test-session", ConversationID: conv, TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	tool := NewLeaveNote(deps)
	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"body":"to whom?","conversation_id":`+jsonStr(conv)+`}`))
	if err != nil {
		t.Fatalf("a refusal must not be an error: %v", err)
	}
	if !strings.Contains(out, "Not sent") || !strings.Contains(out, "ambiguous") {
		t.Errorf("expected an ambiguity refusal; got %q", out)
	}
	if !strings.Contains(out, "alice") || !strings.Contains(out, "bob") {
		t.Errorf("the refusal must name the candidates; got %q", out)
	}
}

// TestLeaveNote_ExplicitToStillWinsInAThread: naming `to` is always honoured.
// The resolution above is a default for an omitted addressee, not an override.
func TestLeaveNote_ExplicitToStillWinsInAThread(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	conv := seedThread(t, store, "alice", "test-session")

	tool := NewLeaveNote(deps)
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"body":"actually for carol","to":"carol","conversation_id":`+jsonStr(conv)+`}`)); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingNotes(context.Background(), collab.Claimant{Name: "carol"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("carol has %d notes, want 1 — an explicit `to` was overridden", len(pending))
	}
}

// TestLeaveNote_NewThreadStillDefaultsToNext: the "next" default is unchanged
// for a note that opens a thread. Only the in-thread case was reinterpreted.
func TestLeaveNote_NewThreadStillDefaultsToNext(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	tool := NewLeaveNote(deps)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"body":"for whoever arrives"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "next session") {
		t.Errorf("a new thread should still default to the next arrival; got %q", out)
	}
	got, err := store.ClaimNotes(context.Background(),
		collab.Claimant{Name: "whoever", ID: "sess-whoever"}, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Addressee != collab.AddresseeNext {
		t.Fatalf("note should carry the 'next' addressee; got %v", got)
	}
}
