package collab

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestConversationPeerParticipant_ResolvesExactlyOneOtherParticipant(t *testing.T) {
	s, ws := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	conv, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob", Body: "question",
		Addressee: "alice", TargetID: "sess-alice", TTL: time.Hour,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice", Body: "answer",
		Addressee: "bob", TargetID: "sess-bob", TTL: time.Hour, ConversationID: conv,
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	peer, found, err := s.ConversationPeerParticipant(ctx, conv, "sess-alice", now.Add(2*time.Second))
	if err != nil || !found || peer.ID != "sess-bob" || peer.Name != "bob" || peer.Workspace != ws {
		t.Fatalf("peer=%#v found=%v err=%v, want stable bob in %s", peer, found, err, ws)
	}
	if peer, found, err := s.ConversationPeerParticipant(
		ctx, conv, "sess-outsider", now.Add(2*time.Second),
	); err != nil || found || peer.ID != "" {
		t.Fatalf("non-participant resolved peer=%#v found=%v err=%v", peer, found, err)
	}

	if _, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "carol", AuthorID: "sess-carol", Body: "interjection",
		Addressee: "alice", TargetID: "sess-alice", TTL: time.Hour, ConversationID: conv,
	}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if peer, found, err := s.ConversationPeerParticipant(
		ctx, conv, "sess-alice", now.Add(3*time.Second),
	); err != nil || found || peer.ID != "" {
		t.Fatalf("ambiguous conversation guessed peer=%#v found=%v err=%v", peer, found, err)
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

func TestConversationSummariesForWorkspace_ScopesGlobalVolume(t *testing.T) {
	global, err := OpenGlobalAt(filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = global.Close() })
	ctx := context.Background()
	now := time.Now()
	conv, err := global.PutNote(ctx, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice", Body: "one", Addressee: "bob", TargetID: "sess-bob",
		TTL: time.Hour, OriginWorkspace: "/workspace/a", TargetWorkspace: "/workspace/b",
	}, now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := global.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob", Body: "two", Addressee: "alice", TargetID: "sess-alice",
		TTL: time.Hour, ConversationID: conv,
		OriginWorkspace: "/workspace/b", TargetWorkspace: "/workspace/a",
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := global.PutNote(ctx, NoteInput{
		AuthorSession: "carol", AuthorID: "sess-carol", Body: "unrelated", Addressee: "dan", TargetID: "sess-dan",
		TTL: time.Hour, OriginWorkspace: "/workspace/c", TargetWorkspace: "/workspace/d",
	}, now); err != nil {
		t.Fatal(err)
	}

	gotA, err := global.ConversationSummariesForWorkspace(ctx, "/workspace/a", now.Add(time.Second), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotA) != 1 || gotA[0].ID != conv || gotA[0].Notes != 1 || gotA[0].Pending != 1 {
		t.Fatalf("target-a summaries = %#v", gotA)
	}
	gotB, err := global.ConversationSummariesForWorkspace(ctx, "/workspace/b", now.Add(time.Second), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotB) != 1 || gotB[0].ID != conv || gotB[0].Notes != 1 || gotB[0].Pending != 1 {
		t.Fatalf("target-b summaries = %#v", gotB)
	}
	if got, err := global.ConversationSummariesForWorkspace(ctx, "/workspace/missing", now.Add(time.Second), 5); err != nil || len(got) != 0 {
		t.Fatalf("unrelated workspace saw global conversations: %#v err=%v", got, err)
	}
}
