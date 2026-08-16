package collab

import (
	"context"
	"testing"
	"time"
)

// observe_test.go pins the counting view. Two properties matter more than the
// arithmetic: it must count PENDING separately from total (a long thread nobody
// is reading is the shape worth seeing), and it must never leak a body — a
// volume view that disclosed content would be a second delivery path around the
// addressee binding.

func TestConversationSummaries_CountsTotalAndPendingSeparately(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	conv, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "first", Addressee: "bob", TTL: time.Hour,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"second", "third"} {
		mustPut(t, s, NoteInput{
			AuthorSession: "alice", AuthorID: "sess-alice",
			Body: body, Addressee: "bob", ConversationID: conv,
		}, now)
	}

	// Bob reads exactly one.
	if got, err := s.ClaimNotes(ctx, Claimant{Name: "bob", ID: "sess-bob"}, now, 1); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 {
		t.Fatalf("claimed %d notes, want 1", len(got))
	}

	sums, err := s.ConversationSummaries(ctx, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 {
		t.Fatalf("summaries = %d, want 1", len(sums))
	}
	if sums[0].ID != conv {
		t.Errorf("id = %q, want %q", sums[0].ID, conv)
	}
	if sums[0].Notes != 3 {
		t.Errorf("Notes = %d, want 3 — delivered notes must still count toward volume", sums[0].Notes)
	}
	if sums[0].Pending != 2 {
		t.Errorf("Pending = %d, want 2", sums[0].Pending)
	}
	if sums[0].LastAt.IsZero() {
		t.Error("LastAt is zero; a stalled thread cannot be told from a live one")
	}
}

// TestConversationSummaries_ExcludesExpired: volume is about LIVE conversations.
// Counting expired rows would make a thread look busy long after both agents
// stopped, which is the opposite of the signal a human is scanning for.
func TestConversationSummaries_ExcludesExpired(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "ancient", Addressee: "bob", TTL: time.Minute,
	}, now.Add(-time.Hour))

	sums, err := s.ConversationSummaries(ctx, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 0 {
		t.Errorf("expired conversation still counted: %+v", sums)
	}
}

// TestConversationSummariesForWorkspace_IsGlobalOnly guards the surface that
// carries the consent rule. Asking a project's own store this question must
// return nothing rather than silently counting nothing, because "no
// cross-project traffic" and "you asked the wrong store" would otherwise be
// indistinguishable to the caller.
func TestConversationSummariesForWorkspace_IsGlobalOnly(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "hello", Addressee: "bob",
	}, now)

	sums, err := s.ConversationSummariesForWorkspace(ctx, "/some/ws", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sums != nil {
		t.Errorf("a non-global store answered a cross-project question: %+v", sums)
	}
}

// TestMergeConversationSummaries_FoldsAThreadSplitAcrossStores: a same-project
// reply lands in the workspace store and a cross-project one in the daemon
// store, both under one conversation_id. Listing them separately would
// overstate the number of conversations and understate each one's length.
func TestMergeConversationSummaries_FoldsAThreadSplitAcrossStores(t *testing.T) {
	early := time.Now().Add(-time.Hour)
	late := time.Now()

	local := []ConversationSummary{
		{ID: "conv-a", Notes: 2, Pending: 1, LastAt: early},
		{ID: "conv-b", Notes: 1, Pending: 0, LastAt: early},
	}
	global := []ConversationSummary{
		{ID: "conv-a", Notes: 3, Pending: 2, LastAt: late},
	}

	got := MergeConversationSummaries(0, local, global)
	if len(got) != 2 {
		t.Fatalf("merged into %d conversations, want 2: %+v", len(got), got)
	}
	if got[0].ID != "conv-a" {
		t.Errorf("busiest conversation = %q, want conv-a", got[0].ID)
	}
	if got[0].Notes != 5 || got[0].Pending != 3 {
		t.Errorf("conv-a = %d notes / %d pending, want 5/3", got[0].Notes, got[0].Pending)
	}
	if !got[0].LastAt.Equal(late) {
		t.Error("LastAt did not take the later of the two halves")
	}
}

// TestMergeConversationSummaries_LimitsAfterMerging: applying the cap per group
// would let a thread's own halves compete for a slot and push the thread out of
// its own ranking.
func TestMergeConversationSummaries_LimitsAfterMerging(t *testing.T) {
	now := time.Now()
	local := []ConversationSummary{
		{ID: "big", Notes: 5, LastAt: now},
		{ID: "small", Notes: 1, LastAt: now},
	}
	global := []ConversationSummary{
		{ID: "big", Notes: 5, LastAt: now},
	}

	got := MergeConversationSummaries(1, local, global)
	if len(got) != 1 {
		t.Fatalf("limit not applied: %+v", got)
	}
	if got[0].ID != "big" || got[0].Notes != 10 {
		t.Errorf("kept %+v, want the merged 'big' with 10 notes", got[0])
	}
}

// TestSentBy_ShowsDeliveredAndUndeliveredNewestFirst is the difference from
// UnreadSentBy: an outbox that showed only failures could not tell a quiet
// session from a broken one.
func TestSentBy_ShowsDeliveredAndUndeliveredNewestFirst(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "older", Addressee: "bob",
	}, now.Add(-time.Minute))
	mustPut(t, s, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "newer", Addressee: "bob",
	}, now)

	// Bob reads one of them, so the outbox holds one of each state.
	if _, err := s.ClaimNotes(ctx, Claimant{Name: "bob", ID: "sess-bob"}, now, 1); err != nil {
		t.Fatal(err)
	}

	sent, err := s.SentBy(ctx, "sess-alice", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 {
		t.Fatalf("SentBy returned %d rows, want 2 — a delivered note must still appear", len(sent))
	}
	if sent[0].Body != "newer" {
		t.Errorf("first row = %q, want the newest (\"newer\")", sent[0].Body)
	}

	// And UnreadSentBy keeps its own contract: unread only, oldest first.
	unread, err := s.UnreadSentBy(ctx, "sess-alice", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 1 {
		t.Fatalf("UnreadSentBy returned %d rows, want 1 — its contract changed", len(unread))
	}
}

// TestSentBy_ScopedToTheCallersOwnRows: the outbox is readable without the
// recipient's consent gate precisely because it holds only what the caller
// wrote. If it ever returned another session's rows, that reasoning collapses.
func TestSentBy_ScopedToTheCallersOwnRows(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()

	mustPut(t, s, NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice", Body: "mine", Addressee: "bob",
	}, now)
	mustPut(t, s, NoteInput{
		AuthorSession: "carol", AuthorID: "sess-carol", Body: "not mine", Addressee: "bob",
	}, now)

	sent, err := s.SentBy(ctx, "sess-alice", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].Body != "mine" {
		t.Fatalf("SentBy leaked another session's rows: %v", bodies(sent))
	}
}
