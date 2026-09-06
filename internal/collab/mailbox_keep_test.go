package collab

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestKeptForever_IsAFarFutureTimestamp pins the sentinel itself: MaxInt64
// nanoseconds, year 2262 — never a zero, which every filter reads as
// already-expired and an older plumb (one a peer's serve proxy can respawn the
// daemon from) would hard-delete on its next prune tick.
func TestKeptForever_IsAFarFutureTimestamp(t *testing.T) {
	if got := keptForever.UnixNano(); got != math.MaxInt64 {
		t.Fatalf("keptForever = %d, want MaxInt64 nanoseconds", got)
	}
	if y := keptForever.Year(); y != 2262 {
		t.Errorf("keptForever year = %d, want 2262 (the last year nanosecond time reaches)", y)
	}
}

// TestClaimNotesKeeping_MakesDeliveredRowImmortal pins the retention contract:
// the TTL bounds a note only while it is unread, the keeping claim supersedes it
// with the far-future stamp in the same statement that marks it read, and Prune
// — which deletes everything past expiry — never catches a kept row. The
// transcript survives the reaper with zero predicate or Prune changes, which is
// the whole design.
func TestClaimNotesKeeping_MakesDeliveredRowImmortal(t *testing.T) {
	s := openKeepTestStore(t)
	ctx := context.Background()
	now := time.Now()

	convID, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob", Body: "kept",
		Addressee: "alice", TTL: time.Hour,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := s.ClaimNotesKeeping(ctx, Claimant{Name: "alice", ID: "sess-alice"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(rows))
	}
	if got := rows[0].ExpiresAt.UnixNano(); got != math.MaxInt64 {
		t.Errorf("kept row expires_at = %d, want the far-future MaxInt64 stamp", got)
	}
	if rows[0].DeliveredAt.IsZero() {
		t.Error("the keeping claim must still stamp the read watermark")
	}

	// A century of reaper ticks must delete nothing...
	if n, err := s.Prune(ctx, now.Add(100*365*24*time.Hour)); err != nil || n != 0 {
		t.Errorf("Prune deleted %d kept rows (err %v); want 0", n, err)
	}
	// ...and the transcript render still sees the row.
	conv, err := s.Conversation(ctx, convID, now.Add(100*365*24*time.Hour))
	if err != nil || len(conv) != 1 {
		t.Errorf("kept transcript row must survive reads: %d rows (err %v)", len(conv), err)
	}
}

// TestClaimNotes_LeavesStoredExpiryAlone: the default claim is
// retention-neutral — a delivered note keeps the expiry it was sent with, so a
// workspace that has not opted into keeping transcripts sees no behaviour
// change at all.
func TestClaimNotes_LeavesStoredExpiryAlone(t *testing.T) {
	s := openKeepTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob", Body: "plain",
		Addressee: "alice", TTL: time.Hour,
	}, now); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ClaimNotes(ctx, Claimant{Name: "alice", ID: "sess-alice"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(rows))
	}
	if got := rows[0].ExpiresAt.Sub(now); got < 55*time.Minute || got > 65*time.Minute {
		t.Errorf("plain claim must leave the stored ~1h expiry; got %s", got)
	}
	if n, err := s.Prune(ctx, now.Add(2*time.Hour)); err != nil || n != 1 {
		t.Errorf("unkept delivered row must still age out: Prune removed %d (err %v)", n, err)
	}
}

// TestPendingCount_MatchesClaimable: the count shares claimable verbatim —
// self-authored rows excluded, everything the claim would hand over counted —
// and after a capped claim it names exactly the remainder.
func TestPendingCount_MatchesClaimable(t *testing.T) {
	s := openKeepTestStore(t)
	ctx := context.Background()
	now := time.Now()
	alice := Claimant{Name: "alice", ID: "sess-alice"}

	for range 4 {
		if _, err := s.PutNote(ctx, NoteInput{
			AuthorSession: "bob", AuthorID: "sess-bob",
			Body: "note", Addressee: "alice", TTL: time.Hour,
		}, now); err != nil {
			t.Fatal(err)
		}
	}
	// A note alice wrote herself is excluded from delivery, so it must be
	// excluded from the count too — a broader count would announce mail the
	// claim refuses, which is the spin loop claimable's comment exists to stop.
	if _, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "self", Addressee: "alice", TTL: time.Hour,
	}, now); err != nil {
		t.Fatal(err)
	}

	if n, err := s.PendingCount(ctx, alice, now); err != nil || n != 4 {
		t.Fatalf("PendingCount = %d (err %v), want 4", n, err)
	}

	rows, err := s.ClaimNotes(ctx, alice, now, 3)
	if err != nil || len(rows) != 3 {
		t.Fatalf("capped claim returned %d rows (err %v), want 3", len(rows), err)
	}
	if n, err := s.PendingCount(ctx, alice, now); err != nil || n != 1 {
		t.Errorf("after a capped claim PendingCount = %d (err %v), want 1", n, err)
	}
	if rows2, err := s.ClaimNotes(ctx, alice, now, 3); err != nil || len(rows2) != 1 {
		t.Errorf("drain claim returned %d rows (err %v), want 1", len(rows2), err)
	}
	if n, err := s.PendingCount(ctx, alice, now); err != nil || n != 0 {
		t.Errorf("empty mailbox PendingCount = %d (err %v), want 0", n, err)
	}
}

// TestPendingCount_ExcludesExpired: an expired-unread row is neither claimable
// nor countable — the reads-filter-regardless invariant, extended to the count,
// so the backlog line can never promise mail a claim would refuse.
func TestPendingCount_ExcludesExpired(t *testing.T) {
	s := openKeepTestStore(t)
	ctx := context.Background()
	now := time.Now()
	alice := Claimant{Name: "alice", ID: "sess-alice"}

	if _, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob",
		Body: "short lived", Addressee: "alice", TTL: time.Minute,
	}, now); err != nil {
		t.Fatal(err)
	}

	if n, err := s.PendingCount(ctx, alice, now); err != nil || n != 1 {
		t.Fatalf("within TTL PendingCount = %d (err %v), want 1", n, err)
	}
	later := now.Add(2 * time.Minute)
	if n, err := s.PendingCount(ctx, alice, later); err != nil || n != 0 {
		t.Errorf("past TTL PendingCount = %d (err %v), want 0", n, err)
	}
	if rows, err := s.ClaimNotes(ctx, alice, later, 0); err != nil || len(rows) != 0 {
		t.Errorf("past TTL claim returned %d rows (err %v), want 0", len(rows), err)
	}
}

// openKeepTestStore is the one-line store every retention test starts from.
func openKeepTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open collab store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
