package collab

import (
	"context"
	"testing"
	"time"
)

// loopback_test.go pins the sender exclusion: a session is never handed a note
// it wrote itself.
//
// The bug this closes is not cosmetic. Delivery is exactly-once, so an author
// that claims its own to:"next" note CONSUMES it — the peer it was written for
// can never receive it, and the sender was told the send succeeded. Nothing
// visible to either agent reveals the loss.

// TestClaimNotes_AuthorNeverReceivesItsOwnNote is the core pin: the author is
// excluded, and the note is still there afterwards for someone else to claim.
// The second half matters as much as the first — an exclusion that dropped the
// row instead of skipping it would pass a test that only checked the author.
func TestClaimNotes_AuthorNeverReceivesItsOwnNote(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	author := Claimant{Name: "alice", ID: "sess-alice"}
	mustPut(t, s, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "whoever picks this up: the migration is half-applied", Addressee: AddresseeNext,
	}, now)

	got, err := s.ClaimNotes(ctx, author, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("author was handed %d of its own notes: %v", len(got), bodies(got))
	}

	// Still deliverable to an actual peer.
	got, err = s.ClaimNotes(ctx, Claimant{Name: "bob", ID: "sess-bob"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("peer read %d notes, want 1 — the exclusion dropped the row instead of skipping it", len(got))
	}
}

// TestHasPendingNotes_AgreesWithClaimForASelfAuthoredNote is the reason the
// exclusion lives in claimable rather than in ClaimNotes. The probe decides
// whether the claim is worth running; if it stayed broader, it would announce
// mail on every tool call that the claim then refuses to hand over — a spin
// loop, and a worse failure than the one being fixed.
func TestHasPendingNotes_AgreesWithClaimForASelfAuthoredNote(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	author := Claimant{Name: "alice", ID: "sess-alice"}
	mustPut(t, s, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "note to nobody", Addressee: AddresseeNext,
	}, now)

	pending, err := s.HasPendingNotes(ctx, author, now)
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("probe reported mail for the author of the only note; the claim will hand it nothing")
	}

	// And it still says yes for someone who really can claim it, so the probe was
	// narrowed rather than broken.
	pending, err = s.HasPendingNotes(ctx, Claimant{Name: "bob", ID: "sess-bob"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("probe reported no mail for a peer that can claim the note")
	}
}

// TestClaimNotes_ExclusionFollowsAnInheritedIdentity: a restarted session that
// inherited its predecessor's mailbox is the same logical agent as the one that
// wrote the note, so the loop must not reopen across a restart.
func TestClaimNotes_ExclusionFollowsAnInheritedIdentity(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice-1",
		Body: "written before the restart", Addressee: AddresseeNext,
	}, now)

	successor := Claimant{Name: "alice", ID: "sess-alice-2", InheritedIDs: []string{"sess-alice-1"}}
	got, err := s.ClaimNotes(ctx, successor, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("restarted session was handed %d notes its predecessor wrote: %v", len(got), bodies(got))
	}
}

// TestClaimNotes_UnattributedNoteStillDelivers guards the empty arm of the
// exclusion. author_id is empty on rows written before senders were attributed,
// and a claimant may hold no ID of its own; a bare `author_id != ?` would read as
// "author_id is not empty" and silently suppress every legacy row for exactly
// the sessions least equipped to notice. A row with no recorded author cannot be
// proven self-authored, so it stays deliverable.
func TestClaimNotes_UnattributedNoteStillDelivers(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{
		AuthorSession: "alice", // no AuthorID — a pre-attribution row
		Body:          "legacy note", Addressee: AddresseeNext,
	}, now)

	for _, who := range []Claimant{
		{Name: "bob", ID: "sess-bob"},
		{Name: "bob"}, // and a claimant with no ID of its own
	} {
		s, _ := openTestStore(t)
		mustPut(t, s, NoteInput{
			AuthorSession: "alice",
			Body:          "legacy note", Addressee: AddresseeNext,
		}, now)

		got, err := s.ClaimNotes(ctx, who, now, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("claimant %+v read %d unattributed notes, want 1", who, len(got))
		}
	}
}

// TestClaimNotes_ExclusionIsByIdentityNotName is the counterpart to the
// addressee_id rule. Two sessions may hold the same name across lifetimes, so
// the exclusion must key on the ID: a genuinely different session that happens
// to answer to the author's name is a legitimate recipient.
func TestClaimNotes_ExclusionIsByIdentityNotName(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice-1",
		Body: "for whoever comes next", Addressee: AddresseeNext,
	}, now)

	// A later, unrelated session that drew the same name. Not the author.
	got, err := s.ClaimNotes(ctx, Claimant{Name: "alice", ID: "sess-alice-2"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a different session sharing the author's name read %d notes, want 1", len(got))
	}
}

// TestClaimNotes_ByNameAddressDoesNotRescueASelfAuthoredNote pins the exclusion
// on the by-name arm too, not only on "next". Being addressed to the claimant's
// current name cannot make a note fresh mail for the session that WROTE it: if
// the name arm overrode the author exclusion, rename_session would launder the
// loopback — write to a peer, adopt the peer's name, and consume the message
// yourself. The author is the same session whatever name it answers to, so the
// identity decides, on both arms alike. (The reverse case — a DIFFERENT session
// answering to the name — is a legitimate recipient, pinned just above.)
func TestClaimNotes_ByNameAddressDoesNotRescueASelfAuthoredNote(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	author := Claimant{Name: "alice", ID: "sess-alice-1"}
	mustPut(t, s, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice-1",
		Body: "addressed to the name I now hold", Addressee: "alice",
	}, now)

	got, err := s.ClaimNotes(ctx, author, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("author claimed %d of its own notes via the by-name arm: %v", len(got), bodies(got))
	}

	// Skipped, not dropped: a different session answering to the name collects it.
	got, err = s.ClaimNotes(ctx, Claimant{Name: "alice", ID: "sess-alice-2"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a different session with the addressee's name read %d notes, want 1", len(got))
	}
}
