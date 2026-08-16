package collab

import (
	"context"
	"path/filepath"
	"slices"
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

// openTestGlobalStore opens a real daemon-level store at a temp path, the same
// helper shape store_test.go's cross-project tests use inline.
func openTestGlobalStore(t *testing.T) *Store {
	t.Helper()
	g, err := OpenGlobalAt(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// TestConversationWorkspaces_CollectsDistinctOriginAndTarget pins the shape
// FilterDaemonWideConversations depends on: a two-sided thread reports both
// participants exactly once, regardless of how many notes each side sent.
func TestConversationWorkspaces_CollectsDistinctOriginAndTarget(t *testing.T) {
	g := openTestGlobalStore(t)
	ctx, now := context.Background(), time.Now()

	conv, err := g.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "b", Body: "hi", Addressee: "alice",
		TTL: time.Hour, OriginWorkspace: "/proj/b", TargetWorkspace: "/proj/a",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, g, NoteInput{
		AuthorSession: "alice", AuthorID: "a", Body: "hi back", Addressee: "bob",
		ConversationID: conv, OriginWorkspace: "/proj/a", TargetWorkspace: "/proj/b",
	}, now)
	// A second reply from bob must not duplicate /proj/b in the result.
	mustPut(t, g, NoteInput{
		AuthorSession: "bob", AuthorID: "b", Body: "again", Addressee: "alice",
		ConversationID: conv, OriginWorkspace: "/proj/b", TargetWorkspace: "/proj/a",
	}, now)

	got, err := g.ConversationWorkspaces(ctx, conv, now)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"/proj/a", "/proj/b"}; !slices.Equal(got, want) {
		t.Errorf("workspaces = %v, want %v", got, want)
	}
}

// TestConversationWorkspaces_ExcludesExpired: an expired row's workspace must
// not count as a live participant.
func TestConversationWorkspaces_ExcludesExpired(t *testing.T) {
	g := openTestGlobalStore(t)
	ctx, now := context.Background(), time.Now()

	conv, err := g.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "b", Body: "hi", Addressee: "alice",
		TTL: time.Minute, OriginWorkspace: "/proj/b", TargetWorkspace: "/proj/a",
	}, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	got, err := g.ConversationWorkspaces(ctx, conv, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expired row's workspaces still counted: %v", got)
	}
}

// TestConversationWorkspaces_UnknownConversationIsEmpty: no rows, no error —
// FilterDaemonWideConversations relies on an empty (not erroring) answer to
// exclude a conversation it cannot place any participant for.
func TestConversationWorkspaces_UnknownConversationIsEmpty(t *testing.T) {
	g := openTestGlobalStore(t)
	got, err := g.ConversationWorkspaces(context.Background(), "no-such-conversation", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("unknown conversation reported workspaces: %v", got)
	}
}

// TestFilterDaemonWideConversations_RequiresUnanimousConsent is the rule
// itself: a conversation is shown only when EVERY participating workspace
// consents, not when any one does.
func TestFilterDaemonWideConversations_RequiresUnanimousConsent(t *testing.T) {
	g := openTestGlobalStore(t)
	ctx, now := context.Background(), time.Now()

	// Both consent: must appear.
	bothConsent, err := g.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "b", Body: "hi", Addressee: "alice",
		TTL: time.Hour, OriginWorkspace: "/proj/consents-a", TargetWorkspace: "/proj/consents-b",
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// One side never consents: must be excluded even though the other does.
	oneRefuses, err := g.PutNote(ctx, NoteInput{
		AuthorSession: "carol", AuthorID: "c", Body: "hi", Addressee: "dave",
		TTL: time.Hour, OriginWorkspace: "/proj/consents-a", TargetWorkspace: "/proj/refuses",
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	consenting := map[string]bool{"/proj/consents-a": true, "/proj/consents-b": true}
	allow := func(ws string) bool { return consenting[ws] }

	got, err := g.FilterDaemonWideConversations(ctx, now, 0, allow)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if !slices.Contains(ids, bothConsent) {
		t.Errorf("a conversation both participants opted in to must appear; got %v", ids)
	}
	if slices.Contains(ids, oneRefuses) {
		t.Errorf("a conversation with even one non-consenting participant must not appear; got %v", ids)
	}
}

// TestFilterDaemonWideConversations_UnplaceableParticipantIsRefused is the
// sharp edge of the unanimous rule: consent from the participants that COULD
// be placed is not consent from the ones that could not.
//
// PutNote requires a target workspace on a global-store row but never an
// origin, and leave_note's cross-project branch stamps origin with the
// SENDER's workspace — which is "" for a session whose workspace never
// resolved, and sameWorkspace("", x) is deliberately false, so that branch is
// exactly what such a session takes. The resulting row names one project and
// leaves the other unplaceable. Displaying it because the placeable half
// consented would be the any-one-consents rule under another name.
func TestFilterDaemonWideConversations_UnplaceableParticipantIsRefused(t *testing.T) {
	g := openTestGlobalStore(t)
	ctx, now := context.Background(), time.Now()

	unplaceable, err := g.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "b", Body: "hi", Addressee: "alice",
		TTL: time.Hour, TargetWorkspace: "/proj/only-target",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// A fully-placed, fully-consenting conversation as the positive control, so
	// this cannot pass by the filter simply returning nothing.
	placed, err := g.PutNote(ctx, NoteInput{
		AuthorSession: "carol", AuthorID: "c", Body: "hi", Addressee: "dave",
		TTL: time.Hour, OriginWorkspace: "/proj/only-target", TargetWorkspace: "/proj/other",
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	allow := func(ws string) bool { return ws == "/proj/only-target" || ws == "/proj/other" }
	got, err := g.FilterDaemonWideConversations(ctx, now, 0, allow)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if slices.Contains(ids, unplaceable) {
		t.Errorf("a conversation with an UNPLACEABLE participant was shown on the placeable half's consent alone: %v", ids)
	}
	if !slices.Contains(ids, placed) {
		t.Errorf("the fully-placed, fully-consenting conversation must still appear: %v", ids)
	}
}

// TestConversationWorkspaces_ReportsAnUnplaceableParticipant pins the
// mechanism the refusal above rests on: the unstamped origin surfaces as "" so
// it reaches the caller's allow func, instead of being filtered away and
// leaving the conversation looking wholly placed.
func TestConversationWorkspaces_ReportsAnUnplaceableParticipant(t *testing.T) {
	g := openTestGlobalStore(t)
	ctx, now := context.Background(), time.Now()

	conv, err := g.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "b", Body: "hi", Addressee: "alice",
		TTL: time.Hour, TargetWorkspace: "/proj/only-target",
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	got, err := g.ConversationWorkspaces(ctx, conv, now)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"", "/proj/only-target"}; !slices.Equal(got, want) {
		t.Errorf("workspaces = %q, want %q — the unplaceable participant must be reported, not dropped", got, want)
	}
}

// TestConversationWorkspaces_OnlyRunsOnGlobalStore: a workspace's own collab.db
// stamps neither column, so answering there would report every conversation as
// a single unplaceable participant. Refuse instead, as
// ConversationSummariesForWorkspace does.
func TestConversationWorkspaces_OnlyRunsOnGlobalStore(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()
	conv, err := s.PutNote(ctx, NoteInput{
		AuthorSession: "alice", AuthorID: "a", Body: "hi", Addressee: "bob", TTL: time.Hour,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.ConversationWorkspaces(ctx, conv, now)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("a non-global store answered a cross-project consent question: %q", got)
	}
}

// TestFilterDaemonWideConversations_OnlyRunsOnGlobalStore: a per-workspace
// store answers nothing rather than silently misreading its own columns —
// mirroring ConversationSummariesForWorkspace's contract.
func TestFilterDaemonWideConversations_OnlyRunsOnGlobalStore(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()
	mustPut(t, s, NoteInput{AuthorSession: "alice", AuthorID: "a", Body: "hi", Addressee: "bob"}, now)

	got, err := s.FilterDaemonWideConversations(ctx, now, 0, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("a non-global store answered a daemon-wide question: %+v", got)
	}
}

// TestFilterDaemonWideConversations_LimitTrimsAfterFiltering proves the
// over-fetch: with several consented conversations available, a small limit
// still returns a full page rather than under-filling because non-consenting
// conversations occupied slots in the raw fetch.
func TestFilterDaemonWideConversations_LimitTrimsAfterFiltering(t *testing.T) {
	g := openTestGlobalStore(t)
	ctx := context.Background()

	// Five non-consenting conversations, newest first (busiest-first ranking is
	// by note count then recency; give them all one note so the refused ones
	// would otherwise crowd out the consented ones in a naive `LIMIT 2` fetch).
	for i := range 5 {
		ws := filepath.Join("/proj/refuses", string(rune('a'+i)))
		if _, err := g.PutNote(ctx, NoteInput{
			AuthorSession: "x", AuthorID: "x", Body: "hi", Addressee: "y",
			TTL: time.Hour, OriginWorkspace: ws, TargetWorkspace: "/proj/refuses-target",
		}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// One consented conversation, oldest of the batch so a naive small LIMIT on
	// the unfiltered query would drop it.
	consented, err := g.PutNote(ctx, NoteInput{
		AuthorSession: "a", AuthorID: "a", Body: "hi", Addressee: "b",
		TTL: time.Hour, OriginWorkspace: "/proj/consents", TargetWorkspace: "/proj/consents-2",
	}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	allow := func(ws string) bool {
		return ws == "/proj/consents" || ws == "/proj/consents-2"
	}
	got, err := g.FilterDaemonWideConversations(ctx, time.Now(), 2, allow)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if !slices.Contains(ids, consented) {
		t.Errorf("the consented conversation was crowded out by refused ones: %v", ids)
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
