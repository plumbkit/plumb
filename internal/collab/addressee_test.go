package collab

import (
	"context"
	"strings"
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

// TestHasPendingNotes_AgreesWithTheClaimInEveryCase is the probe's whole
// contract. It exists only to decide whether ClaimNotes is worth running, so a
// disagreement is a delivery bug that presents as silence: a probe broader than
// the claim promises mail that never arrives, and one narrower suppresses a
// claim that would have delivered. Neither raises an error, so the agreement has
// to be pinned rather than assumed.
//
// The probe runs BEFORE the claim in each case, which is also the assertion that
// it mutates nothing: were it to consume, the claim behind it would come back
// empty and every "want delivery" row would fail.
func TestHasPendingNotes_AgreesWithTheClaimInEveryCase(t *testing.T) {
	const myWS, otherWS = "/proj/mine", "/proj/theirs"
	me := Claimant{Name: "alice", ID: "sess-alice-1", Workspace: myWS}

	cases := []struct {
		name     string
		note     NoteInput
		age      time.Duration // how long before "now" the note was written
		claimant Claimant
		want     bool
	}{
		{"addressed to me, unbound", NoteInput{Addressee: "alice"}, 0, me, true},
		{"addressed to me and bound to me", NoteInput{Addressee: "alice", AddresseeID: "sess-alice-1"}, 0, me, true},
		{"bound to my predecessor", NoteInput{Addressee: "alice", AddresseeID: "sess-alice-0"}, 0, me, false},
		{"addressed to someone else", NoteInput{Addressee: "bob"}, 0, me, false},
		{"next arrival", NoteInput{Addressee: AddresseeNext}, 0, me, true},
		{"expired", NoteInput{Addressee: "alice"}, 2 * time.Hour, me, false},
		{
			"cross-project, my workspace",
			NoteInput{Addressee: "alice", TargetWorkspace: myWS, OriginWorkspace: otherWS},
			0, me, true,
		},
		{
			"cross-project, another workspace",
			NoteInput{Addressee: "alice", TargetWorkspace: otherWS, OriginWorkspace: myWS},
			0, me, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			ctx, now := context.Background(), time.Now()
			in := tc.note
			in.AuthorID, in.Body, in.TTL = "id-sender", tc.name, time.Hour
			mustPut(t, s, in, now.Add(-tc.age))

			probed, err := s.HasPendingNotes(ctx, tc.claimant, now)
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := s.ClaimNotes(ctx, tc.claimant, now, 0)
			if err != nil {
				t.Fatal(err)
			}
			delivered := len(claimed) > 0

			if delivered != tc.want {
				t.Fatalf("ClaimNotes delivered=%v, want %v — the fixture, not the probe, is wrong", delivered, tc.want)
			}
			if probed != delivered {
				t.Errorf("probe said %v but the claim delivered %v; a probe that disagrees with the "+
					"claim it guards silently drops or invents mail", probed, delivered)
			}
		})
	}
}

// TestHasPendingNotes_GoesQuietOnceTheMessageIsClaimed: the probe reads the same
// watermark the claim sets, so a delivered message must stop waking the response
// path. Otherwise every tool call after the first delivery would probe true and
// then claim nothing — the exact cost the probe was added to remove, restored.
func TestHasPendingNotes_GoesQuietOnceTheMessageIsClaimed(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()
	me := Claimant{Name: "alice", ID: "sess-alice"}

	mustPut(t, s, NoteInput{AuthorID: "id-bob", Body: "read me", Addressee: "alice"}, now)

	if ok, err := s.HasPendingNotes(ctx, me, now); err != nil || !ok {
		t.Fatalf("probe before the claim = %v (err %v), want true", ok, err)
	}
	if _, err := s.ClaimNotes(ctx, me, now, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasPendingNotes(ctx, me, now); err != nil || ok {
		t.Fatalf("probe after the claim = %v (err %v), want false", ok, err)
	}
}

// TestClaimant_InheritedIDsReadTheirPredecessorAndNothingWider is the store's
// half of the inheritance contract. An inherited identity must do exactly one
// thing — let a reconnected session collect mail bound to the session it
// continues — and must not become a general widening of what it may read.
func TestClaimant_InheritedIDsReadTheirPredecessorAndNothingWider(t *testing.T) {
	const myWS, otherWS = "/proj/mine", "/proj/theirs"
	// A session that came back through the authenticated reconnect: new own ID,
	// its predecessor inherited.
	heir := Claimant{Name: "alice", ID: "sess-alice-2", InheritedIDs: []string{"sess-alice-1"}, Workspace: myWS}

	cases := []struct {
		name string
		note NoteInput
		want bool
	}{
		{"bound to the inherited predecessor", NoteInput{Addressee: "alice", AddresseeID: "sess-alice-1"}, true},
		{"bound to this session itself", NoteInput{Addressee: "alice", AddresseeID: "sess-alice-2"}, true},
		{"unbound", NoteInput{Addressee: "alice"}, true},
		{"bound to a session it never was", NoteInput{Addressee: "alice", AddresseeID: "sess-someone-else"}, false},
		{"addressed to another name", NoteInput{Addressee: "bob", AddresseeID: "sess-alice-1"}, false},
		{
			"cross-project to another workspace, bound to the predecessor",
			NoteInput{Addressee: "alice", AddresseeID: "sess-alice-1", TargetWorkspace: otherWS, OriginWorkspace: myWS},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			ctx, now := context.Background(), time.Now()
			in := tc.note
			in.AuthorID, in.Body, in.TTL = "id-bob", tc.name, time.Hour
			mustPut(t, s, in, now)

			probed, err := s.HasPendingNotes(ctx, heir, now)
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := s.ClaimNotes(ctx, heir, now, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(claimed) > 0; got != tc.want {
				t.Fatalf("claim delivered=%v, want %v", got, tc.want)
			}
			// The probe guards the claim, so a disagreement here is a delivery bug
			// even when the claim itself is right.
			if probed != (len(claimed) > 0) {
				t.Errorf("probe said %v but the claim delivered %v", probed, len(claimed) > 0)
			}
		})
	}
}

// TestClaimant_InheritedIDsDoNotWidenNext: "next" is a first-claimer race and
// always carries an empty addressee_id, so it is matched by the unbound arm no
// matter how many identities a claimant presents. A heir must win it on exactly
// the same terms as anyone else — once, and not twice for having two IDs.
func TestClaimant_InheritedIDsDoNotWidenNext(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()
	mustPut(t, s, NoteInput{AuthorID: "id-bob", Body: "for the next arrival", Addressee: AddresseeNext}, now)

	heir := Claimant{Name: "alice", ID: "sess-2", InheritedIDs: []string{"sess-1"}}
	got, err := s.ClaimNotes(ctx, heir, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a heir claimed %d 'next' notes, want exactly 1", len(got))
	}
	again, err := s.ClaimNotes(ctx, heir, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("the heir claimed the same 'next' note again (%d) — extra identities must not "+
			"multiply a row into several deliveries", len(again))
	}
}

// TestPendingNotes_ListsInheritedMailToo keeps the third reader in step. The
// listing applies the same identity set as the claim, or a reconnected session
// is told its mailbox is empty while the delivery paths hand it messages.
func TestPendingNotes_ListsInheritedMailToo(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()
	mustPut(t, s, NoteInput{
		AuthorID: "id-bob", Body: "bound to the predecessor",
		Addressee: "alice", AddresseeID: "sess-alice-1",
	}, now)

	heir := Claimant{Name: "alice", ID: "sess-alice-2", InheritedIDs: []string{"sess-alice-1"}}
	listed, err := s.PendingNotes(ctx, heir, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("the listing showed %v, want the inherited message", bodies(listed))
	}

	// A session with no inheritance still sees nothing, so the listing widened
	// for the heir alone.
	stranger := Claimant{Name: "alice", ID: "sess-alice-2"}
	listed, err = s.PendingNotes(ctx, stranger, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("a session without the inheritance was shown %v", bodies(listed))
	}
}

// TestClaimant_IdentitiesRejectsBlanksAndDuplicates: the IN list is built from
// this, so a blank slipping in would be bound as a parameter and a duplicate
// would widen the statement for nothing. Neither changes what matches — the
// unbound arm already covers ” — but the list is the security-relevant part of
// the predicate and it should carry only what it means.
func TestClaimant_IdentitiesRejectsBlanksAndDuplicates(t *testing.T) {
	who := Claimant{ID: "own", InheritedIDs: []string{"", "prev", "prev", "own"}}
	got := who.identities()
	want := []string{"own", "prev"}
	if len(got) != len(want) {
		t.Fatalf("identities() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("identities() = %v, want %v", got, want)
		}
	}
}

// TestAddresseeMatch_InterpolatesNoData is what the two //nolint:gosec G202
// directives in mailbox.go rest on. Session IDs became part of the statement's
// SHAPE (the length of the IN list) when a claimant gained several identities,
// so the query text is now assembled at runtime. The safety property is that
// only the COUNT crosses into the SQL — every value is bound — and a property
// asserted in a comment is one a later edit can quietly break.
func TestAddresseeMatch_InterpolatesNoData(t *testing.T) {
	hostile := Claimant{
		Name: "alice",
		ID:   `' OR 1=1 --`,
		InheritedIDs: []string{
			`"; DROP TABLE collab_rows; --`,
			"sess-legitimate",
		},
	}

	// The precise property: no caller VALUE appears in the statement text. The
	// generated SQL does legitimately contain '' — the fixed unbound-row literal —
	// so a blanket "no quotes" check would be testing the wrong thing.
	values := append([]string{hostile.ID, hostile.Name}, hostile.InheritedIDs...)

	idSQL, args := addresseeMatch(hostile)
	for _, v := range values {
		if strings.Contains(idSQL, v) {
			t.Fatalf("generated SQL carries the caller value %q: %q", v, idSQL)
		}
	}
	if got, want := strings.Count(idSQL, "?"), len(args); got != want {
		t.Fatalf("%d placeholders for %d arguments — a mismatch binds the wrong values", got, want)
	}
	if len(args) != 3 {
		t.Fatalf("args = %v, want all three identities bound as parameters", args)
	}

	// And the whole predicate composes the same way.
	where, whereArgs := claimable(hostile, time.Now())
	for _, v := range values {
		if strings.Contains(where, v) {
			t.Fatalf("claimable() carries the caller value %q: %q", v, where)
		}
	}
	if got, want := strings.Count(where, "?"), len(whereArgs); got != want {
		t.Fatalf("claimable(): %d placeholders for %d arguments", got, want)
	}

	// End to end: a hostile identity must simply match nothing, not error and not
	// return somebody else's row.
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()
	mustPut(t, s, NoteInput{AuthorID: "id-bob", Body: "bound elsewhere", Addressee: "alice", AddresseeID: "sess-other"}, now)
	got, err := s.ClaimNotes(ctx, hostile, now, 0)
	if err != nil {
		t.Fatalf("a hostile identity must be inert, not an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("hostile identity claimed %v", bodies(got))
	}
}
