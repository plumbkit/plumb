package collab

import (
	"context"
	"sync"
	"testing"
	"time"
)

// addressee_test.go pins the addressee_id binding: the rule that a message left
// for a peer belongs to THAT session, not to whoever later answers to its name.
//
// The hole it closes is name reuse across session lifetimes. Names are drawn
// from a pool of a few thousand, an ended session does not reserve its name, and
// rename_session lets a live session adopt any free one — so a note whose
// recipient exits before reading it used to be handed, body and all, to the next
// holder of that name, with the sender told it reached the peer it meant.

// mustPut stores a note and fails the test if the store refuses it.
func mustPut(t *testing.T, s *Store, in NoteInput, now time.Time) {
	t.Helper()
	if in.TTL == 0 {
		in.TTL = time.Hour
	}
	if _, err := s.PutNote(context.Background(), in, now); err != nil {
		t.Fatalf("PutNote(%+v): %v", in, err)
	}
}

// TestClaimNotes_BoundNoteIsInvisibleToANameReuser is the anti-impersonation
// pin. The successor presents the right name and the right workspace and still
// reads nothing, because the row names the session it was written for.
func TestClaimNotes_BoundNoteIsInvisibleToANameReuser(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{
		AuthorSession: "bob", AuthorID: "id-bob", Body: "the staging credentials are rotated",
		Addressee: "alice", AddresseeID: "sess-alice-1",
	}, now)

	// alice's session ends. A new one draws — or renames to — the same name.
	for _, impostor := range []Claimant{
		{Name: "alice", ID: "sess-alice-2"},
		{Name: "alice"}, // and presenting no id at all must not pass as "unbound"
	} {
		got, err := s.ClaimNotes(ctx, impostor, now, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("claimant %+v read %d of its predecessor's messages: %v", impostor, len(got), bodies(got))
		}
	}

	// The binding narrows delivery; it does not break it. The session the message
	// was actually written for still receives it.
	got, err := s.ClaimNotes(ctx, Claimant{Name: "alice", ID: "sess-alice-1"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "the staging credentials are rotated" {
		t.Fatalf("the intended session claimed %v, want its one message", bodies(got))
	}
	if got[0].AddresseeID != "sess-alice-1" {
		t.Errorf("AddresseeID = %q, want the binding to survive the round trip", got[0].AddresseeID)
	}
}

// TestClaimNotes_UnboundNotesStillDeliverByName is the compatibility half, and
// it matters at least as much as the binding: an unbound row is every message
// written before this column existed, every message to a peer that had not
// attached yet, and every "next" note. If binding had made those undeliverable,
// the fix would have silently emptied every mailbox already in flight.
func TestClaimNotes_UnboundNotesStillDeliverByName(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{AuthorID: "id-bob", Body: "unbound, addressed by name", Addressee: "alice"}, now)

	got, err := s.ClaimNotes(ctx, Claimant{Name: "alice", ID: "any-session-at-all"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "unbound, addressed by name" {
		t.Fatalf("an unbound note must still deliver by name; got %v", bodies(got))
	}
	if got[0].AddresseeID != "" {
		t.Errorf("AddresseeID = %q, want empty — nothing should invent a binding", got[0].AddresseeID)
	}
}

// TestPutNote_NextIsNeverBound: "whoever attaches next" is a race by design, so
// a binding on it would be a race whose winner was chosen in advance — and, for
// a caller that passed its own id by mistake, one nobody but the sender could
// win. The store clears it rather than trusting callers not to set it.
func TestPutNote_NextIsNeverBound(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{
		AuthorID: "id-bob", Body: "for the next arrival",
		Addressee: AddresseeNext, AddresseeID: "sess-bob",
	}, now)

	got, err := s.ClaimNotes(ctx, Claimant{Name: "stranger", ID: "sess-stranger"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a 'next' note must be claimable by any arrival; got %v", bodies(got))
	}
	if got[0].AddresseeID != "" {
		t.Errorf("stored AddresseeID = %q on a 'next' note, want it cleared", got[0].AddresseeID)
	}
}

// TestClaimNotes_NextStillRacesToExactlyOneWinner: the claim is one atomic
// UPDATE matching delivered_at = 0, and adding an identity term to its WHERE
// clause must not have loosened that into a double delivery or tightened it into
// none. Unlike the existing concurrency pin, every claimant here has a DISTINCT
// session id, which is the dimension the new predicate reads.
func TestClaimNotes_NextStillRacesToExactlyOneWinner(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{AuthorID: "id-bob", Body: "first come", Addressee: AddresseeNext}, now)

	const claimants = 16
	var (
		mu    sync.Mutex
		won   int
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	for i := range claimants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			who := Claimant{Name: "arrival-" + string(rune('a'+i)), ID: "sess-" + string(rune('a'+i))}
			rows, err := s.ClaimNotes(ctx, who, now, 0)
			if err != nil {
				t.Errorf("ClaimNotes: %v", err)
				return
			}
			mu.Lock()
			won += len(rows)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if won != 1 {
		t.Fatalf("a 'next' note was delivered %d times, want exactly 1", won)
	}
}

// TestPendingNotes_AppliesTheSamePredicateAsTheClaim: the listing path must not
// advertise what the claiming path would refuse. Showing a name-reuser the
// pending mail of its predecessor discloses the sender and the body — the very
// content the binding exists to protect — while promising a message it can never
// read.
func TestPendingNotes_AppliesTheSamePredicateAsTheClaim(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{
		AuthorSession: "bob", AuthorID: "id-bob", Body: "bound body",
		Addressee: "alice", AddresseeID: "sess-alice-1",
	}, now)
	mustPut(t, s, NoteInput{AuthorID: "id-bob", Body: "unbound body", Addressee: "alice"}, now)

	pending, err := s.PendingNotes(ctx, Claimant{Name: "alice", ID: "sess-alice-2"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != "unbound body" {
		t.Fatalf("a name-reuser was listed %v, want only the unbound message", bodies(pending))
	}

	pending, err = s.PendingNotes(ctx, Claimant{Name: "alice", ID: "sess-alice-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("the intended session was listed %v, want both messages", bodies(pending))
	}
}

// TestUnreadSentBy_IsTheSendersOwnOutboxOnly. The receipt exists so a sender can
// tell "read and not answered" from "never read". It is addressed by author_id —
// always a session ID — so it must show a session its own unread messages and
// nobody else's: a query that leaked a peer's outbox would hand over the bodies
// of messages this session was never party to.
func TestUnreadSentBy_IsTheSendersOwnOutboxOnly(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{AuthorSession: "me", AuthorID: "id-me", Body: "mine, unread", Addressee: "alice"}, now)
	mustPut(t, s, NoteInput{AuthorSession: "peer", AuthorID: "id-peer", Body: "not mine", Addressee: "alice"}, now)

	got, err := s.UnreadSentBy(ctx, "id-me", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "mine, unread" {
		t.Fatalf("outbox = %v, want only this session's own message", bodies(got))
	}
}

// TestUnreadSentBy_DropsAMessageOnceItIsRead, and — the load-bearing half —
// NEVER sets the watermark itself. A receipt that claimed on the sender's behalf
// would consume a message the recipient has not seen, turning delivered-exactly-
// once into delivered-never. So the recipient must still be able to claim after
// any number of receipt reads.
func TestUnreadSentBy_IsAReadAndNeverAClaim(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{AuthorSession: "me", AuthorID: "id-me", Body: "please read this", Addressee: "alice"}, now)

	for i := range 3 {
		got, err := s.UnreadSentBy(ctx, "id-me", now, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("read #%d: outbox = %d, want the message to stay unread", i, len(got))
		}
	}

	// The recipient can still collect it — the receipt consumed nothing.
	claimed, err := s.ClaimNotes(ctx, Claimant{Name: "alice", ID: "sess-alice"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Body != "please read this" {
		t.Fatalf("the recipient claimed %v — a receipt read must not burn the watermark", bodies(claimed))
	}

	// And now the sender's outbox goes quiet, because it really was read.
	got, err := s.UnreadSentBy(ctx, "id-me", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("outbox still lists %v after the message was read", bodies(got))
	}
}
