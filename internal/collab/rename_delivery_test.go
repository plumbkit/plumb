package collab

import (
	"context"
	"testing"
	"time"
)

// rename_delivery_test.go pins the rule that delivery follows the session a note
// was BOUND to, not the name that session happened to answer to when it was
// written.
//
// The field report this came from (2026-09-15): a session reconnected, its
// identity recovery could not reapply its stored name, and it came back under a
// generated one with its internal session ID unchanged. A peer's note, bound to
// exactly that ID, then became unreachable by every receive path while the
// sender saw a successful send — and because mail bound to a session expires
// unread rather than passing to a later holder of the name, it was lost rather
// than delayed.

// TestClaimNotes_BoundNoteSurvivesARename is the headline regression. It is
// written from the real row: same addressee_id, different current name.
func TestClaimNotes_BoundNoteSurvivesARename(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	const id = "sess-gentle-mink"
	mustPut(t, s, NoteInput{
		AuthorSession: "ancient-stag", AuthorID: "id-stag",
		Body:      "written while it answered to gentle-mink",
		Addressee: "gentle-mink", AddresseeID: id,
	}, now)

	// Same session, same ID, regenerated display name.
	got, err := s.ClaimNotes(ctx, Claimant{Name: "icy-beaver", ID: id}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("claimed %v, want the one note bound to this session's own ID — a regenerated "+
			"display name must not strand mail the row already proves is its own", bodies(got))
	}
}

// TestHasPendingNotes_AgreesWithTheClaimAcrossARename: the probe guards the
// claim, so a probe that still keys on the name reports nothing while the claim
// would deliver — the idle-wake hook then never fires for exactly the message
// that most needs it. Asserted separately because claimable() being shared is a
// property of today's code, not a guarantee.
func TestHasPendingNotes_AgreesWithTheClaimAcrossARename(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	const id = "sess-gentle-mink"
	mustPut(t, s, NoteInput{
		AuthorSession: "ancient-stag", AuthorID: "id-stag", Body: "x",
		Addressee: "gentle-mink", AddresseeID: id,
	}, now)

	renamed := Claimant{Name: "icy-beaver", ID: id}
	pending, err := s.HasPendingNotes(ctx, renamed, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNotes(ctx, renamed, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !pending || len(claimed) != 1 {
		t.Fatalf("probe=%v claim=%d, want true/1 — probe and claim must answer the same question", pending, len(claimed))
	}
}

// TestPendingNotes_ListsBoundMailAfterARename covers the listing surface. It
// carries its own narrowing ("next" is excluded), so it builds the predicate
// separately and can drift from the claim independently.
func TestPendingNotes_ListsBoundMailAfterARename(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	const id = "sess-gentle-mink"
	mustPut(t, s, NoteInput{
		AuthorSession: "ancient-stag", AuthorID: "id-stag", Body: "bound body",
		Addressee: "gentle-mink", AddresseeID: id,
	}, now)

	listed, err := s.PendingNotes(ctx, Claimant{Name: "icy-beaver", ID: id}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %v, want the bound note — a session told its mailbox is empty while "+
			"the claim would hand it a message is the disagreement this fixes", bodies(listed))
	}
}

// TestPendingNotes_StillExcludesNext is the other half of that listing's
// contract, and the reason PendingNotes passes includeNext=false. Dropping the
// distinction would advertise a first-claimer race the caller may lose.
func TestPendingNotes_StillExcludesNext(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{AuthorID: "id-bob", Body: "for whoever arrives", Addressee: AddresseeNext}, now)

	listed, err := s.PendingNotes(ctx, Claimant{Name: "alice", ID: "sess-alice"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("the listing showed a \"next\" note: %v", bodies(listed))
	}
	// And the claim still takes it, so the exclusion is the listing's alone.
	claimed, err := s.ClaimNotes(ctx, Claimant{Name: "alice", ID: "sess-alice"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("the claim must still take a \"next\" note; got %v", bodies(claimed))
	}
}

// TestClaimNotes_RenameDoesNotWidenPastTheBinding is the security baseline the
// fix must not cost. A renamed session gains its OWN bound mail and nothing
// else: a stranger answering to the addressee's name still reads nothing, and
// neither does a session presenting no identity at all.
func TestClaimNotes_RenameDoesNotWidenPastTheBinding(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{
		AuthorSession: "ancient-stag", AuthorID: "id-stag", Body: "secret",
		Addressee: "gentle-mink", AddresseeID: "sess-gentle-mink",
	}, now)

	for _, impostor := range []Claimant{
		{Name: "gentle-mink", ID: "sess-someone-else"}, // right name, wrong identity
		{Name: "gentle-mink"},                          // right name, no identity
		{Name: "icy-beaver", ID: "sess-unrelated"},     // neither
	} {
		got, err := s.ClaimNotes(ctx, impostor, now, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("claimant %+v read a note bound to another session: %v", impostor, bodies(got))
		}
	}

	// THE POSITIVE CONTROL, and it is not decoration. Every assertion above is of
	// the form "got nothing", which a universally-empty result satisfies — a
	// predicate typo, a fixture that never inserted, a misconfigured store. Without
	// a claimant that MUST receive, this test passes just as happily when delivery
	// is broken for everyone, and it is the test the PR description offers as the
	// proof that the widening is safe.
	//
	// The control is the headline case itself: the rightful owner under a
	// regenerated name. So the same loop proves the guard holds and the feature
	// works, and neither can be true vacuously while the other is asserted.
	owner, err := s.ClaimNotes(ctx, Claimant{Name: "icy-beaver", ID: "sess-gentle-mink"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(owner) != 1 || owner[0].Body != "secret" {
		t.Fatalf("the rightful owner claimed %v, want the one note bound to it — without this the "+
			"three refusals above are satisfied by delivery being broken for everybody", bodies(owner))
	}
}

// TestClaimableNotes_CountsBoundMailAfterARename is the probe surface the idle
// wake hook and the message preview are built on (cli.mailWaiting, and the
// preview's Peek). It builds on claimable() today, but that is an implementation
// fact, so the widening is asserted here rather than inferred from ClaimNotes —
// this is the layer a caller above it inherits.
func TestClaimableNotes_CountsBoundMailAfterARename(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	const id = "sess-gentle-mink"
	mustPut(t, s, NoteInput{
		AuthorSession: "ancient-stag", AuthorID: "id-stag", Body: "bound body",
		Addressee: "gentle-mink", AddresseeID: id,
	}, now)

	listed, err := s.ClaimableNotes(ctx, Claimant{Name: "icy-beaver", ID: id}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("the probe counted %v, want the bound note — a probe that still keys on the "+
			"name leaves the wake hook silent for exactly the mail that needs it", bodies(listed))
	}

	// And it stays closed to a stranger holding the addressee's name, so the
	// widening the preview inherits is the owner's alone.
	listed, err = s.ClaimableNotes(ctx, Claimant{Name: "gentle-mink", ID: "sess-stranger"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("the probe counted a stranger's view of a bound note: %v", bodies(listed))
	}
}

// TestClaimableNotes_LimitDoesNotHideBoundMailBehindNewerUnboundMail is the
// interaction between this predicate and the cap Peek passes it.
//
// The probe orders ORDER BY created_at ASC and applies LIMIT, so what survives a
// cap is decided by AGE, not by how a row is addressed. That is the property
// worth pinning: widening delivery to bound rows would be hollow if a cap then
// preferred newer unbound rows and the bound one — the older, and the one whose
// loss is silent and permanent — were the row that got cut.
//
// It is the same shape that silently disarmed the probe/claim mirror fixture,
// where a bound row appended last was cut by benchClaimLimit before either
// statement saw it.
func TestClaimableNotes_LimitDoesNotHideBoundMailBehindNewerUnboundMail(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	const id = "sess-gentle-mink"
	// Oldest: bound to this session under the name it answered to back then.
	mustPut(t, s, NoteInput{
		AuthorSession: "ancient-stag", AuthorID: "id-stag", Body: "bound and oldest",
		Addressee: "gentle-mink", AddresseeID: id,
	}, now.Add(-3*time.Minute))
	// Newer, unbound, addressed to the name it answers to NOW.
	for i, age := range []time.Duration{-2 * time.Minute, -time.Minute} {
		mustPut(t, s, NoteInput{
			AuthorID: "id-stag", Body: string(rune('a'+i)) + " newer unbound", Addressee: "icy-beaver",
		}, now.Add(age))
	}

	renamed := Claimant{Name: "icy-beaver", ID: id}
	capped, err := s.ClaimableNotes(ctx, renamed, now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 1 || capped[0].Body != "bound and oldest" {
		t.Fatalf("a cap of 1 returned %v, want the oldest row — which is the bound one. A cap "+
			"that prefers newer unbound mail hides the row whose loss is permanent", bodies(capped))
	}

	// Uncapped, everything the session is entitled to is there.
	all, err := s.ClaimableNotes(ctx, renamed, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("uncapped = %v, want all three", bodies(all))
	}
}
