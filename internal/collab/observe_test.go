package collab

import (
	"context"
	"testing"
	"time"
)

func TestConversationPeer_ResolvesExactlyOneOtherParticipant(t *testing.T) {
	s, ws := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	conv, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob", Body: "question",
		Addressee: "alice", TTL: time.Hour,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice", Body: "answer",
		Addressee: "bob", TTL: time.Hour, ConversationID: conv,
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	peer, found, err := s.ConversationPeer(ctx, conv, "sess-alice", "alice", now.Add(2*time.Second))
	if err != nil || !found || peer != "bob" {
		t.Fatalf("peer=%q found=%v err=%v, want bob", peer, found, err)
	}

	if _, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "carol", AuthorID: "sess-carol", Body: "interjection",
		Addressee: "alice", TTL: time.Hour, ConversationID: conv,
	}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if peer, found, err := s.ConversationPeer(
		ctx, conv, "sess-alice", "alice", now.Add(3*time.Second),
	); err != nil || found || peer != "" {
		t.Fatalf("ambiguous conversation guessed peer=%q found=%v err=%v", peer, found, err)
	}

	// The workspace value is used below to make sure the store remains live; it
	// also documents that peer resolution never needs a second database.
	if ws == "" {
		t.Fatal("test store workspace unexpectedly empty")
	}
}

func TestConversationObservability_TracksLivePendingAndDelivered(t *testing.T) {
	s, ws := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	conv, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice", Body: "one",
		Addressee: "bob", TTL: time.Hour,
	}, now.Add(-2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice", Body: "two",
		Addressee: "bob", TTL: time.Hour, ConversationID: conv,
	}, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice", Body: "expired",
		Addressee: "bob", TTL: time.Minute, ConversationID: conv,
	}, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ClaimNotesForSession(ctx, "bob", "sess-bob", ws, now, 1); err != nil || len(rows) != 1 {
		t.Fatalf("claim one: rows=%#v err=%v", rows, err)
	}

	sent, err := s.RecentSentNotes(ctx, "sess-alice", now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 {
		t.Fatalf("live sent notes = %d, want 2", len(sent))
	}
	delivered, pending := 0, 0
	for _, row := range sent {
		if row.DeliveredAt.IsZero() {
			pending++
		} else {
			delivered++
		}
	}
	if delivered != 1 || pending != 1 {
		t.Fatalf("delivery states: delivered=%d pending=%d, want 1/1", delivered, pending)
	}

	summaries, err := s.ConversationSummaries(ctx, now, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].ID != conv ||
		summaries[0].Notes != 2 || summaries[0].Pending != 1 {
		t.Fatalf("summaries = %#v", summaries)
	}
}
