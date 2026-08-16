package collab

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	ws := t.TempDir()
	s, err := Open(ws)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, ws
}

func TestExists_LazyCreation(t *testing.T) {
	ws := t.TempDir()
	if Exists(ws) {
		t.Fatal("Exists should be false before any collab feature is used")
	}
	s, err := Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !Exists(ws) {
		t.Fatal("Exists should be true after Open")
	}
}

func TestOpen_WritesGitignore(t *testing.T) {
	s, ws := openTestStore(t)
	_ = s
	data, err := os.ReadFile(filepath.Join(ws, ".plumb", ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	for _, want := range []string{"collab.db", "collab.db-wal", "collab.db-shm"} {
		if !strings.Contains(string(data), want) {
			t.Errorf(".gitignore missing %q; got:\n%s", want, data)
		}
	}
}

func TestPutIntent_ReplacesPriorPerSession(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	put := func(id, body string) {
		if err := s.PutIntent(ctx, IntentInput{AuthorSession: "sess-" + id, AuthorID: id, Body: body, TTL: time.Hour}, now); err != nil {
			t.Fatalf("PutIntent: %v", err)
		}
	}
	put("A", "first")
	put("A", "second") // replaces A's intent
	put("B", "other")

	intents, err := s.LiveIntents(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 2 {
		t.Fatalf("live intents = %d, want 2 (one per session)", len(intents))
	}
	bodies := map[string]string{}
	for _, r := range intents {
		bodies[r.AuthorID] = r.Body
	}
	if bodies["A"] != "second" {
		t.Errorf("A's live intent = %q, want the replacement %q", bodies["A"], "second")
	}
	if bodies["B"] != "other" {
		t.Errorf("B's live intent = %q, want %q", bodies["B"], "other")
	}
}

func TestLiveIntents_FiltersExpired(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.PutIntent(ctx, IntentInput{AuthorID: "A", Body: "x", TTL: 5 * time.Minute}, now); err != nil {
		t.Fatal(err)
	}
	// Query as if 10 minutes have passed — the row is expired and filtered.
	future := now.Add(10 * time.Minute)
	intents, err := s.LiveIntents(ctx, future)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 0 {
		t.Fatalf("expired intent should be filtered on read; got %d", len(intents))
	}
}

// TestClaimNotes_DeliversEachMessageExactlyOnce pins the read watermark, which
// replaced delete-on-delivery: a claimed message is not handed over a second
// time — to the same session or to any other — but it stays in the table so the
// conversation keeps a transcript and a countable number of exchanges.
func TestClaimNotes_DeliversEachMessageExactlyOnce(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	mustNote := func(to, body string) {
		if _, err := s.PutNote(ctx, NoteInput{AuthorSession: "author", AuthorID: "au", Body: body, Addressee: to, TTL: time.Hour}, now); err != nil {
			t.Fatalf("PutNote: %v", err)
		}
	}
	mustNote(AddresseeNext, "for whoever attaches next")
	mustNote("alice", "hello alice")

	// alice reads first: she gets her own note plus the next-arrival one.
	got, err := s.ClaimNotes(ctx, Claimant{Name: "alice"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("alice claim = %d messages, want 2 (addressed + next)", len(got))
	}

	// bob reads later: the next note was already claimed, and alice's is not his.
	got2, err := s.ClaimNotes(ctx, Claimant{Name: "bob"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 0 {
		t.Fatalf("bob claim = %d messages, want 0 (next already claimed, addressed note not his)", len(got2))
	}

	// alice reading again gets nothing: the watermark, not deletion, is what stops
	// a re-delivery.
	again, err := s.ClaimNotes(ctx, Claimant{Name: "alice"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second claim returned %d messages, want 0 — a delivered message must not repeat", len(again))
	}

	// Nothing is pending for her either, but the rows survive for the transcript.
	pending, err := s.PendingNotes(ctx, Claimant{Name: "alice"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("alice pending = %d, want 0 once claimed", len(pending))
	}
	if n, err := s.ConversationCount(ctx, got[1].ConversationID); err != nil || n != 1 {
		t.Fatalf("delivered message should remain countable: n=%d err=%v", n, err)
	}
}

// TestClaimNotes_OrdersOldestFirst: a conversation must read in the order it was
// written, not newest-first like the intent listing.
func TestClaimNotes_OrdersOldestFirst(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for i, body := range []string{"first", "second", "third"} {
		if _, err := s.PutNote(ctx, NoteInput{AuthorID: "au", Body: body, Addressee: "alice", TTL: time.Hour},
			now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ClaimNotes(ctx, Claimant{Name: "alice"}, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Body != "first" || got[2].Body != "third" {
		t.Fatalf("claim order = %v, want first→third", bodies(got))
	}
}

// TestPutNote_ThreadsAndMintsConversations: omitting a conversation id starts a
// thread; passing one continues it, which is what the exchange budget counts.
func TestPutNote_ThreadsAndMintsConversations(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	first, err := s.PutNote(ctx, NoteInput{AuthorID: "au", Body: "q", Addressee: "alice", TTL: time.Hour}, now)
	if err != nil || first == "" {
		t.Fatalf("PutNote should mint a conversation id: %q %v", first, err)
	}
	second, err := s.PutNote(ctx, NoteInput{AuthorID: "au2", Body: "a", Addressee: "bob", TTL: time.Hour, ConversationID: first}, now)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("reply conversation = %q, want the original %q", second, first)
	}
	other, err := s.PutNote(ctx, NoteInput{AuthorID: "au", Body: "unrelated", Addressee: "alice", TTL: time.Hour}, now)
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("a fresh message must not reuse an existing conversation id")
	}
	if n, err := s.ConversationCount(ctx, first); err != nil || n != 2 {
		t.Fatalf("conversation count = %d (err %v), want 2", n, err)
	}
}

// TestPutNote_ExchangeBudgetHoldsUnderConcurrency is the regression pin for the
// check-then-insert race an independent review found. The budget used to be a
// caller-side ConversationCount followed by a separate PutNote; two agents
// replying into one thread at the same instant both read one-below-the-limit and
// both landed, so the cap over-ran exactly when the exchange was running away.
// Only an atomic check-and-insert holds it.
func TestPutNote_ExchangeBudgetHoldsUnderConcurrency(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	const limit, repliers = 5, 16

	reply := func(conv string) (string, error) {
		return s.PutNote(ctx, NoteInput{
			AuthorID: "au", Body: "m", Addressee: "alice", TTL: time.Hour,
			ConversationID: conv, MaxExchanges: limit,
		}, now)
	}
	conv, err := reply("")
	if err != nil {
		t.Fatalf("opening a thread must never be refused: %v", err)
	}

	var (
		mu                sync.Mutex
		accepted, refused int
		unexpected        []error
	)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range repliers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := reply(conv)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrConversationFull):
				refused++
			default:
				unexpected = append(unexpected, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(unexpected) > 0 {
		t.Fatalf("%d concurrent replies failed for a reason other than the budget (first: %v)",
			len(unexpected), unexpected[0])
	}
	n, err := s.ConversationCount(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if n != limit {
		t.Errorf("conversation holds %d messages, want exactly the %d-message cap — a budget "+
			"that over-runs under concurrency is no budget at all", n, limit)
	}
	if accepted != limit-1 || refused != repliers-(limit-1) {
		t.Errorf("accepted %d / refused %d, want %d / %d — every over-budget reply must be "+
			"refused with ErrConversationFull, not silently stored",
			accepted, refused, limit-1, repliers-(limit-1))
	}
}

// TestPutNote_BudgetIgnoresExpiredMessages: the budget counts live rows only, so
// it does not matter whether the reaper has run. Counting expired-but-unpruned
// rows would make a thread's remaining allowance depend on reaper timing —
// refused now, allowed a tick later once the same rows are deleted.
func TestPutNote_BudgetIgnoresExpiredMessages(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	const limit = 2

	// A thread spent long ago: minTTL-lived rows written two hours back, so they
	// are expired but still present until the next prune.
	stale := now.Add(-2 * time.Hour)
	conv, err := s.PutNote(ctx, NoteInput{
		AuthorID: "au", Body: "old q", Addressee: "alice", TTL: time.Minute, MaxExchanges: limit,
	}, stale)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutNote(ctx, NoteInput{
		AuthorID: "au", Body: "old a", Addressee: "bob", TTL: time.Minute,
		ConversationID: conv, MaxExchanges: limit,
	}, stale); err != nil {
		t.Fatal(err)
	}

	if _, err := s.PutNote(ctx, NoteInput{
		AuthorID: "au", Body: "new", Addressee: "alice", TTL: time.Hour,
		ConversationID: conv, MaxExchanges: limit,
	}, now); err != nil {
		t.Fatalf("expired messages must not spend a budget the next prune would refund: %v", err)
	}
	n, err := s.ConversationCount(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 — only the unexpired message is still part of the thread", n)
	}
	// Pruning away the expired rows must be invisible to both the count and the
	// budget: that equivalence is the whole point.
	if _, err := s.Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
	after, err := s.ConversationCount(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if after != n {
		t.Errorf("count went %d → %d across a prune; the budget must not depend on the reaper", n, after)
	}
}

// TestClaimNotes_LimitLeavesRemainderUnclaimed is the anti-message-loss pin.
// Claiming marks a row delivered for good, so the per-call cap MUST be applied
// by the query — if it were applied by trimming the returned slice, the trimmed
// messages would be marked delivered and never shown to anyone.
func TestClaimNotes_LimitLeavesRemainderUnclaimed(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for i, body := range []string{"one", "two", "three", "four", "five"} {
		if _, err := s.PutNote(ctx, NoteInput{AuthorID: "au", Body: body, Addressee: "alice", TTL: time.Hour},
			now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	read := now.Add(time.Minute)

	first, err := s.ClaimNotes(ctx, Claimant{Name: "alice"}, read, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Body != "one" || first[1].Body != "two" {
		t.Fatalf("first claim = %v, want the two oldest", bodies(first))
	}

	// The other three must still be waiting, not silently consumed.
	rest, err := s.ClaimNotes(ctx, Claimant{Name: "alice"}, read, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 3 {
		t.Fatalf("remainder = %v, want the 3 messages the cap did not reach", bodies(rest))
	}
}

// TestClaimNotes_ConcurrentClaimersDoNotError is the regression pin for the bug
// an independent review found. The claim used to be a DEFERRED transaction that
// SELECTed and then UPDATEd; in WAL mode SQLite cannot upgrade a stale read
// snapshot to a write, so it failed with SQLITE_BUSY_SNAPSHOT — which
// busy_timeout does NOT retry. Delivery is precisely where sessions wake
// together (one send bumps every watcher), and the error was silently swallowed,
// so an agent simply never got its message. Before the fix this failed on
// essentially every burst.
func TestClaimNotes_ConcurrentClaimersDoNotError(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	base := time.Now()

	var mu sync.Mutex
	var failures []error
	claimed := 0

	const rounds, perRound, claimers = 25, 5, 3
	for range rounds {
		for range perRound {
			if _, err := s.PutNote(ctx, NoteInput{AuthorID: "a", Body: "m", Addressee: "alice", TTL: time.Hour}, base); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range claimers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rows, err := s.ClaimNotes(ctx, Claimant{Name: "alice"}, base.Add(time.Minute), 2)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failures = append(failures, err)
					return
				}
				claimed += len(rows)
			}()
		}
		close(start)
		wg.Wait()
	}
	if len(failures) > 0 {
		t.Fatalf("%d concurrent claims errored (first: %v) — delivery must tolerate simultaneous readers",
			len(failures), failures[0])
	}
	// Drain whatever the capped claims left, then assert nothing was lost or
	// double-delivered across the whole run.
	rest, err := s.ClaimNotes(ctx, Claimant{Name: "alice"}, base.Add(time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	if total := claimed + len(rest); total != rounds*perRound {
		t.Errorf("delivered %d messages, want exactly %d — none lost, none twice", total, rounds*perRound)
	}
}

// TestClaimNotes_TargetWorkspaceScopesCrossProjectMail pins the fix for the
// interception hole: the daemon-level store is shared by every project, and a
// session NAME is not a safe address (names come from a small pool with no
// uniqueness check, and rename_session lets a session pick any). A row must be
// claimable only by a session pinned to the workspace it names.
func TestClaimNotes_TargetWorkspaceScopesCrossProjectMail(t *testing.T) {
	g, err := OpenGlobalAt(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	ctx := context.Background()
	now := time.Now()

	if _, err := g.PutNote(ctx, NoteInput{
		AuthorSession: "carol", AuthorID: "c", Body: "secret", Addressee: "alice",
		TTL: time.Hour, OriginWorkspace: "/proj/c", TargetWorkspace: "/proj/a",
	}, now); err != nil {
		t.Fatal(err)
	}

	// An impostor in another project, using the same session name.
	got, err := g.ClaimNotes(ctx, Claimant{Name: "alice", Workspace: "/proj/evil"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a session in another workspace claimed %d cross-project message(s) by name alone", len(got))
	}

	// The real recipient still gets it.
	got, err = g.ClaimNotes(ctx, Claimant{Name: "alice", Workspace: "/proj/a"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the intended recipient claimed %d, want 1", len(got))
	}
}

// TestPutNote_GlobalStoreRefusesUnaddressableRows: the daemon-level store keeps
// its own invariants rather than trusting callers — a row with no target could
// be claimed by anyone, and "next" has no meaning across projects.
func TestPutNote_GlobalStoreRefusesUnaddressableRows(t *testing.T) {
	g, err := OpenGlobalAt(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	ctx, now := context.Background(), time.Now()

	if _, err := g.PutNote(ctx, NoteInput{AuthorID: "c", Body: "x", Addressee: "alice", TTL: time.Hour}, now); err == nil {
		t.Error("a cross-project note with no target workspace must be refused")
	}
	if _, err := g.PutNote(ctx, NoteInput{
		AuthorID: "c", Body: "x", Addressee: AddresseeNext, TTL: time.Hour, TargetWorkspace: "/proj/a",
	}, now); err == nil {
		t.Error(`"next" must be refused by the cross-project store`)
	}
}

// TestConversationPeerWorkspace_PlacesAnOfflinePeer backs the routing fix: a
// reply must still reach a peer that disconnected between turns, instead of
// silently landing in the sender's own mailbox.
func TestConversationPeerWorkspace_PlacesAnOfflinePeer(t *testing.T) {
	g, err := OpenGlobalAt(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	ctx, now := context.Background(), time.Now()

	conv, err := g.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "b", Body: "hi", Addressee: "alice",
		TTL: time.Hour, OriginWorkspace: "/proj/b", TargetWorkspace: "/proj/a",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if ws, ok := g.ConversationPeerWorkspace(ctx, conv, "bob"); !ok || ws != "/proj/b" {
		t.Errorf("conversation should place bob at /proj/b; got %q ok=%v", ws, ok)
	}
	if _, ok := g.ConversationPeerWorkspace(ctx, conv, "nobody"); ok {
		t.Error("an unknown peer must not be placed")
	}
}

func bodies(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Body
	}
	return out
}

func TestPendingNotes_AddresseeMatchOnly(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	_, _ = s.PutNote(ctx, NoteInput{AuthorID: "au", Body: "n1", Addressee: "alice", TTL: time.Hour}, now)
	_, _ = s.PutNote(ctx, NoteInput{AuthorID: "au", Body: "n2", Addressee: AddresseeNext, TTL: time.Hour}, now)

	pending, err := s.PendingNotes(ctx, Claimant{Name: "alice"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending for alice = %d, want 1 (a next note is not listed here)", len(pending))
	}
	if pending[0].Addressee != "alice" {
		t.Errorf("pending addressee = %q, want alice", pending[0].Addressee)
	}
}

func TestPutNote_DefaultsToNext(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := s.PutNote(ctx, NoteInput{AuthorID: "au", Body: "hi", Addressee: "", TTL: time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimNotes(ctx, Claimant{Name: "whoever"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Addressee != AddresseeNext {
		t.Fatalf("empty addressee should default to %q; got %v", AddresseeNext, got)
	}
}

func TestPrune_RemovesExpired(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	_ = s.PutIntent(ctx, IntentInput{AuthorID: "A", Body: "x", TTL: 5 * time.Minute}, now)
	_, _ = s.PutNote(ctx, NoteInput{AuthorID: "A", Body: "y", Addressee: "bob", TTL: 5 * time.Minute}, now)

	n, err := s.Prune(ctx, now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d rows, want 2", n)
	}
	// A fresh row survives a prune at present time.
	_ = s.PutIntent(ctx, IntentInput{AuthorID: "B", Body: "z", TTL: time.Hour}, now)
	n2, err := s.Prune(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("pruned %d unexpired rows, want 0", n2)
	}
}

func TestClearSessionIntents_LeavesNotes(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	_ = s.PutIntent(ctx, IntentInput{AuthorID: "sess1", Body: "refactoring", TTL: time.Hour}, now)
	_, _ = s.PutNote(ctx, NoteInput{AuthorID: "sess1", Body: "note survives", Addressee: "peer", TTL: time.Hour}, now)

	if err := s.ClearSessionIntents(ctx, "sess1"); err != nil {
		t.Fatal(err)
	}
	intents, _ := s.LiveIntents(ctx, now)
	if len(intents) != 0 {
		t.Fatalf("session's intent should be cleared on close; got %d", len(intents))
	}
	notes, _ := s.PendingNotes(ctx, Claimant{Name: "peer"}, now)
	if len(notes) != 1 {
		t.Fatalf("notes must survive their author; got %d", len(notes))
	}
}

func TestClampTTL_FloorsShortTTL(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	// A zero TTL would store an already-expired row; clampTTL floors it to minTTL.
	if err := s.PutIntent(ctx, IntentInput{AuthorID: "A", Body: "x", TTL: 0}, now); err != nil {
		t.Fatal(err)
	}
	intents, err := s.LiveIntents(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 {
		t.Fatalf("a zero-TTL intent should still live at least minTTL; got %d", len(intents))
	}
}

// TestOpenGlobalReadOnly_ReadsWithoutCreatingOrWriting pins the TUI's read
// path: it must see rows a writer (e.g. leave_note through the daemon) put
// there, must never create the file when absent, and must refuse a write.
func TestOpenGlobalReadOnly_ReadsWithoutCreatingOrWriting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	if _, err := OpenGlobalReadOnly(); err == nil {
		t.Fatal("OpenGlobalReadOnly must error, not create, when collab-xproject.db does not exist")
	}
	if GlobalExists() {
		t.Fatal("a failed OpenGlobalReadOnly must not have created the global store")
	}

	w, err := OpenGlobal()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, now := context.Background(), time.Now()
	if _, err := w.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "b", Body: "hi", Addressee: "alice",
		TTL: time.Hour, OriginWorkspace: "/proj/b", TargetWorkspace: "/proj/a",
	}, now); err != nil {
		t.Fatal(err)
	}

	r, err := OpenGlobalReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sums, err := r.ConversationSummaries(ctx, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 {
		t.Fatalf("read-only handle did not see the written note: %+v", sums)
	}
	if _, err := r.PutNote(ctx, NoteInput{
		AuthorID: "x", Body: "should fail", Addressee: "y", TTL: time.Hour, TargetWorkspace: "/proj/a",
	}, now); err == nil {
		t.Error("a read-only handle must refuse a write")
	}
}
