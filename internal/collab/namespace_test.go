package collab

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// namespace_test.go pins the one property namespaces exist for: a store is shared
// by every namespace, but a ROW belongs to the namespace it was written in, and
// every read, claim, count, update and delete a namespaced view makes sees only
// its own. Each test writes the same shape of data into several namespaces of ONE
// store and asserts each reader sees exactly its own slice — never "nothing", which
// a broken fixture would also produce — so every negative has a positive control
// in the same test.

// nsAll are the namespaces the tests populate: the default and two named ones.
var nsAll = []string{"", "ns-a", "ns-b"}

// nsViews returns one Store per namespace over base's single database. The default
// namespace is base itself, as production uses it.
func nsViews(base *Store) map[string]*Store {
	views := map[string]*Store{"": base}
	for _, ns := range nsAll[1:] {
		views[ns] = base.InNamespace(ns)
	}
	return views
}

// rawNote inserts a note straight into a namespace, for the shapes PutNote's own
// guards would refuse (a thread id shared across namespaces, a stranger in a
// thread). Written directly because the point is to build the cross-namespace
// collision the scoping has to survive.
func rawNote(t *testing.T, s *Store, ns, conv, authorID, body string, now time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO collab_rows (kind, author_session, author_id, body, addressee,
		created_at, expires_at, conversation_id, namespace)
		VALUES ('note', ?, ?, ?, 'someone', ?, ?, ?, ?)`,
		authorID, authorID, body, now.UnixNano(), now.Add(time.Hour).UnixNano(), conv, ns); err != nil {
		t.Fatal(err)
	}
}

// rowsIn counts every row of one namespace, expired or not, straight from the table.
func rowsIn(t *testing.T, s *Store, ns string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM collab_rows WHERE namespace = ?`, ns).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wantBodies(t *testing.T, what string, got []Row, want ...string) {
	t.Helper()
	if !slices.Equal(bodies(got), want) {
		t.Errorf("%s = %v, want %v", what, bodies(got), want)
	}
}

// TestNamespace_MailIsInvisibleOutsideItsNamespace is the delivery pin. One note
// per namespace, all to the same addressee, in the three addressing shapes a row
// can have. Every delivery read — the probe, the listing, the count, the snapshot,
// the pure read and the claim — must agree that a reader holds exactly the note its
// own namespace wrote, in both directions: a named namespace cannot read the
// default one's mail, and the default cannot read a named one's.
func TestNamespace_MailIsInvisibleOutsideItsNamespace(t *testing.T) {
	variants := []struct {
		name string
		note NoteInput
		// pending is what PendingNotes lists for a reader holding its own note: it
		// never lists a "next" note (see its doc), so that arm lists none.
		pending int
	}{
		{"unbound by name", NoteInput{Addressee: "alice"}, 1},
		{"bound to the session id", NoteInput{Addressee: "alice", AddresseeID: "id-alice"}, 1},
		{"to next", NoteInput{Addressee: AddresseeNext}, 0},
	}
	me := Claimant{Name: "alice", ID: "id-alice"}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			base, _ := openTestStore(t)
			views := nsViews(base)
			ctx, now := context.Background(), time.Now()
			for _, ns := range nsAll {
				in := v.note
				in.AuthorSession, in.AuthorID, in.Body, in.TTL = "bob", "id-bob", "from-"+ns, time.Hour
				mustPut(t, views[ns], in, now)
			}

			own := readerCounts{probe: 1, claimable: 1, pending: v.pending, count: 1, snapshot: 1}
			for _, ns := range nsAll {
				if got := countsFor(t, views[ns], me, now); got != own {
					t.Errorf("namespace %q reader counts = %+v, want %+v — it must see its own note and no other's", ns, got, own)
				}
				listed, err := views[ns].ClaimableNotes(ctx, me, now, 0)
				if err != nil {
					t.Fatal(err)
				}
				wantBodies(t, "ClaimableNotes in "+ns, listed, "from-"+ns)
			}

			// Claiming in one namespace must leave the others' mail unread: the first
			// claim would otherwise take every namespace's note at once.
			for _, ns := range nsAll {
				got, err := views[ns].ClaimNotes(ctx, me, now, 0)
				if err != nil {
					t.Fatal(err)
				}
				wantBodies(t, "ClaimNotes in "+ns, got, "from-"+ns)
				again, err := views[ns].ClaimNotes(ctx, me, now, 0)
				if err != nil || len(again) != 0 {
					t.Errorf("a second claim in %q = %v (err %v), want none", ns, bodies(again), err)
				}
			}
		})
	}
}

// TestNamespace_ANamespacedWriteIsInvisibleToTheDefaultAlone is the minimal shape
// the task states, kept apart as the plainest reading of the rule: a note written
// by one named view is not claimable, pending or counted by the default claimant
// nor by another named one, and is by its own (the positive control).
func TestNamespace_ANamespacedWriteIsInvisibleToTheDefaultAlone(t *testing.T) {
	base, _ := openTestStore(t)
	a, b := base.InNamespace("ns-a"), base.InNamespace("ns-b")
	ctx, now := context.Background(), time.Now()
	mustPut(t, a, NoteInput{AuthorID: "id-bob", Body: "for a", Addressee: "alice"}, now)
	me := Claimant{Name: "alice", ID: "id-alice"}

	for name, reader := range map[string]*Store{"default": base, "ns-b": b} {
		if got := countsFor(t, reader, me, now); got != (readerCounts{}) {
			t.Errorf("%s reader sees ns-a's note: %+v", name, got)
		}
		if got, err := reader.ClaimNotes(ctx, me, now, 0); err != nil || len(got) != 0 {
			t.Errorf("%s reader claimed ns-a's note: %v (err %v)", name, bodies(got), err)
		}
	}
	// The foreign claims above must not have consumed it.
	if got := countsFor(t, a, me, now); got != (readerCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("ns-a reader (positive control) = %+v, want its own note intact", got)
	}
	got, err := a.ClaimNotes(ctx, me, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantBodies(t, "ns-a claim", got, "for a")
}

// TestNamespace_GlobalStoreMailIsScoped is the same property on the daemon-level
// store, where the read scope also carries a target workspace and, without
// consent, a repository-family restriction. Both arms must keep the namespace.
func TestNamespace_GlobalStoreMailIsScoped(t *testing.T) {
	repo := scopeRepo(t, t.TempDir())
	wt := scopeWorktree(t, filepath.Join(repo, ".git"), "wt", filepath.Join(t.TempDir(), "wt"))
	for _, consent := range []bool{true, false} {
		name := "family without consent"
		if consent {
			name = "with consent"
		}
		t.Run(name, func(t *testing.T) {
			g := openScopeGlobal(t)
			views := nsViews(g)
			now := time.Now()
			for _, ns := range nsAll {
				if _, err := views[ns].PutNote(context.Background(), NoteInput{
					AuthorSession: "bob", AuthorID: "id-bob", Body: "from-" + ns, Addressee: "alice",
					TTL: time.Hour, OriginWorkspace: wt, TargetWorkspace: repo,
				}, now); err != nil {
					t.Fatal(err)
				}
			}
			me := Claimant{Name: "alice", ID: "id-alice", Workspace: repo, CrossProject: consent}
			for _, ns := range nsAll {
				if got := countsFor(t, views[ns], me, now); got != (readerCounts{1, 1, 1, 1, 1}) {
					t.Errorf("namespace %q reader counts = %+v, want only its own note", ns, got)
				}
				got, err := views[ns].ClaimNotes(context.Background(), me, now, 0)
				if err != nil {
					t.Fatal(err)
				}
				wantBodies(t, "ClaimNotes in "+ns, got, "from-"+ns)
			}
		})
	}
}

// TestNamespace_OwnedOriginsAreLookedUpPerNamespace pins the family lookup itself.
// The delivery reads above would still return the right rows if it ignored the
// namespace — the outer query filters again — so this asks it directly: the
// default namespace holds a family-stamped note, and a named view must find no
// sending repository because of it.
func TestNamespace_OwnedOriginsAreLookedUpPerNamespace(t *testing.T) {
	repo := scopeRepo(t, t.TempDir())
	wt := scopeWorktree(t, filepath.Join(repo, ".git"), "wt", filepath.Join(t.TempDir(), "wt"))
	g := openScopeGlobal(t)
	now, ctx := time.Now(), context.Background()
	sendFrom(t, g, wt, repo, "family note in the default namespace", now)

	got, err := g.ownedOrigins(ctx, repo, now, true)
	if err != nil || len(got) != 1 {
		t.Fatalf("default view origins = %v (err %v), want the one sending repository", got, err)
	}
	for _, ns := range nsAll[1:] {
		other, err := g.InNamespace(ns).ownedOrigins(ctx, repo, now, true)
		if err != nil || len(other) != 0 {
			t.Errorf("%q view origins = %v (err %v), want none from another namespace's mail", ns, other, err)
		}
	}
}

// TestNamespace_IntentsAreScoped covers the broadcast side. The same author id is
// used in every namespace on purpose: an intent is replaced and cleared by author,
// so an unscoped DELETE would let one namespace's session erase another's.
func TestNamespace_IntentsAreScoped(t *testing.T) {
	base, _ := openTestStore(t)
	views := nsViews(base)
	ctx, now := context.Background(), time.Now()
	first := func(ns string) string { return "first in [" + ns + "]" }
	for _, ns := range nsAll {
		if err := views[ns].PutIntent(ctx, IntentInput{
			AuthorSession: "carol", AuthorID: "id-carol", Body: first(ns), TTL: time.Hour,
		}, now); err != nil {
			t.Fatal(err)
		}
	}

	for _, ns := range nsAll {
		live, err := views[ns].LiveIntents(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		wantBodies(t, "LiveIntents in "+ns, live, first(ns))
	}

	// A replacement in one namespace replaces only its own intent.
	if err := views["ns-a"].PutIntent(ctx, IntentInput{
		AuthorSession: "carol", AuthorID: "id-carol", Body: "second in ns-a", TTL: time.Hour,
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for ns, want := range map[string]string{"": first(""), "ns-a": "second in ns-a", "ns-b": first("ns-b")} {
		live, err := views[ns].LiveIntents(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		wantBodies(t, "LiveIntents in "+ns+" after ns-a replaced its own", live, want)
	}

	// Clearing a session in one namespace clears only that namespace's intent.
	if err := views["ns-b"].ClearSessionIntents(ctx, "id-carol"); err != nil {
		t.Fatal(err)
	}
	for ns, want := range map[string][]string{"": {first("")}, "ns-a": {"second in ns-a"}, "ns-b": nil} {
		live, err := views[ns].LiveIntents(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		wantBodies(t, "LiveIntents in "+ns+" after ns-b cleared its session", live, want...)
	}
}

// TestNamespace_ThreadReadsAreScoped covers the transcript, the routing lookup and
// the count: a thread written in ns-a is not there for anyone else.
func TestNamespace_ThreadReadsAreScoped(t *testing.T) {
	base, _ := openTestStore(t)
	a := base.InNamespace("ns-a")
	ctx, now := context.Background(), time.Now()
	conv, err := a.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "id-bob", Body: "hello", Addressee: "alice",
		TTL: time.Hour, OriginWorkspace: "/where/bob/is",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	me := Claimant{Name: "alice", ID: "id-alice"}

	for name, reader := range map[string]*Store{"default": base, "ns-b": base.InNamespace("ns-b")} {
		if rows, err := reader.Conversation(ctx, conv, now); err != nil || len(rows) != 0 {
			t.Errorf("%s Conversation = %v (err %v), want none", name, bodies(rows), err)
		}
		if rows, err := reader.ConversationFor(ctx, conv, me, now); err != nil || len(rows) != 0 {
			t.Errorf("%s ConversationFor = %v (err %v), want none", name, bodies(rows), err)
		}
		if n, err := reader.ConversationCount(ctx, conv); err != nil || n != 0 {
			t.Errorf("%s ConversationCount = %d (err %v), want 0", name, n, err)
		}
		if ws, ok := reader.ConversationPeerWorkspace(ctx, conv, "bob"); ok || ws != "" {
			t.Errorf("%s ConversationPeerWorkspace = %q, %v, want none", name, ws, ok)
		}
	}

	rows, err := a.Conversation(ctx, conv, now)
	if err != nil {
		t.Fatal(err)
	}
	wantBodies(t, "ns-a Conversation (positive control)", rows, "hello")
	if n, err := a.ConversationCount(ctx, conv); err != nil || n != 1 {
		t.Errorf("ns-a ConversationCount = %d (err %v), want 1", n, err)
	}
	if ws, ok := a.ConversationPeerWorkspace(ctx, conv, "bob"); !ok || ws != "/where/bob/is" {
		t.Errorf("ns-a ConversationPeerWorkspace = %q, %v, want bob's workspace", ws, ok)
	}
}

// TestNamespace_OutboxIsScoped: a session's sent-notes view is its own namespace's
// rows. The author id is shared across namespaces to show it is the namespace,
// not the id, that separates them.
func TestNamespace_OutboxIsScoped(t *testing.T) {
	base, _ := openTestStore(t)
	views := nsViews(base)
	ctx, now := context.Background(), time.Now()
	for _, ns := range nsAll {
		mustPut(t, views[ns], NoteInput{AuthorSession: "bob", AuthorID: "id-bob", Body: "sent-" + ns, Addressee: "alice"}, now)
	}
	for _, ns := range nsAll {
		sent, err := views[ns].SentBy(ctx, "id-bob", now, 0)
		if err != nil {
			t.Fatal(err)
		}
		wantBodies(t, "SentBy in "+ns, sent, "sent-"+ns)
		unread, err := views[ns].UnreadSentBy(ctx, "id-bob", now, 0)
		if err != nil {
			t.Fatal(err)
		}
		wantBodies(t, "UnreadSentBy in "+ns, unread, "sent-"+ns)
	}
}

// TestNamespace_ConversationSummariesAreScoped covers the volume views. alice is a
// member of the same thread id in ns-a and in the default namespace's lookalike, so
// the membership subselect has to be namespaced too, or the default view would
// report a thread alice is only in elsewhere.
func TestNamespace_ConversationSummariesAreScoped(t *testing.T) {
	base, _ := openTestStore(t)
	a := base.InNamespace("ns-a")
	ctx, now := context.Background(), time.Now()
	me := Claimant{Name: "alice", ID: "id-alice"}

	// ns-a: alice is in thread c-shared. Default: the same id, but alice is not in it.
	rawNote(t, a, "ns-a", "c-shared", "id-alice", "alice in ns-a", now)
	rawNote(t, base, "ns-a", "c-shared", "id-bob", "bob in ns-a", now)
	rawNote(t, base, "", "c-shared", "id-carol", "carol in default", now)
	rawNote(t, base, "", "c-shared", "id-dave", "dave in default", now)

	inA, err := a.ConversationSummaries(ctx, me, now, 0)
	if err != nil || len(inA) != 1 || inA[0].Notes != 2 {
		t.Fatalf("ns-a summaries = %+v (err %v), want the one two-note thread", inA, err)
	}
	inDefault, err := base.ConversationSummaries(ctx, me, now, 0)
	if err != nil || len(inDefault) != 0 {
		t.Fatalf("default summaries for alice = %+v (err %v), want none: she is not in the default namespace's c-shared", inDefault, err)
	}
	// The unscoped view counts a namespace's whole traffic, and only that.
	all, err := base.conversationSummaries(ctx, now, 0, nil, nil)
	if err != nil || len(all) != 1 || all[0].Notes != 2 {
		t.Fatalf("default unscoped summaries = %+v (err %v), want the default thread's two notes only", all, err)
	}
}

// TestNamespace_GlobalObservabilityIsScoped is the daemon-level side of the volume
// views: per-workspace counts, a thread's participating workspaces, and the
// operator's consent-filtered listing all stay inside the namespace.
func TestNamespace_GlobalObservabilityIsScoped(t *testing.T) {
	g := openTestGlobalStore(t)
	a := g.InNamespace("ns-a")
	ctx, now := context.Background(), time.Now()
	conv, err := a.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "id-bob", Body: "x-project", Addressee: "alice",
		TTL: time.Hour, OriginWorkspace: "/proj/from", TargetWorkspace: "/proj/to",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	reader := Claimant{Workspace: "/proj/to", CrossProject: true}
	allow := func(string) bool { return true }

	if got, err := g.ConversationSummariesForWorkspace(ctx, reader, now, 0); err != nil || len(got) != 0 {
		t.Errorf("default ConversationSummariesForWorkspace = %+v (err %v), want none", got, err)
	}
	if got, err := g.ConversationWorkspaces(ctx, conv, now); err != nil || len(got) != 0 {
		t.Errorf("default ConversationWorkspaces = %v (err %v), want none", got, err)
	}
	if got, err := g.FilterDaemonWideConversations(ctx, now, 0, allow); err != nil || len(got) != 0 {
		t.Errorf("default FilterDaemonWideConversations = %+v (err %v), want none", got, err)
	}
	// The listing above would stay empty even if the underlying count ignored the
	// namespace, because ConversationWorkspaces drops what it cannot place — so the
	// unscoped count is asked directly.
	if got, err := g.conversationSummaries(ctx, now, 0, nil, nil); err != nil || len(got) != 0 {
		t.Errorf("default unscoped summaries = %+v (err %v), want none", got, err)
	}

	if got, err := a.ConversationSummariesForWorkspace(ctx, reader, now, 0); err != nil || len(got) != 1 {
		t.Errorf("ns-a ConversationSummariesForWorkspace = %+v (err %v), want its thread", got, err)
	}
	if got, err := a.ConversationWorkspaces(ctx, conv, now); err != nil || len(got) != 2 {
		t.Errorf("ns-a ConversationWorkspaces = %v (err %v), want both workspaces", got, err)
	}
	if got, err := a.FilterDaemonWideConversations(ctx, now, 0, allow); err != nil || len(got) != 1 {
		t.Errorf("ns-a FilterDaemonWideConversations = %+v (err %v), want its thread", got, err)
	}
}

// TestNamespace_ThreadingIntoAnotherNamespacesThreadIsRefused: a conversation id is
// an address within one namespace. alice is a member of c-shared in ns-a only, so
// in the default namespace she is a stranger to the thread of the same id and must
// be told so — including when a budget is set, where a membership check that
// ignored the namespace would instead mis-report the refusal as a full thread.
func TestNamespace_ThreadingIntoAnotherNamespacesThreadIsRefused(t *testing.T) {
	for _, maxExchanges := range []int{0, 100} {
		base, _ := openTestStore(t)
		ctx, now := context.Background(), time.Now()
		rawNote(t, base, "ns-a", "c-shared", "id-alice", "alice in ns-a", now)
		rawNote(t, base, "", "c-shared", "id-carol", "carol in default", now)

		_, err := base.PutNote(ctx, NoteInput{
			AuthorSession: "alice", AuthorID: "id-alice", Body: "barging in", Addressee: "carol",
			ConversationID: "c-shared", MaxExchanges: maxExchanges,
		}, now)
		if !errors.Is(err, ErrNotAParticipant) {
			t.Errorf("max_exchanges %d: threading into another namespace's thread = %v, want ErrNotAParticipant", maxExchanges, err)
		}

		// Positive control: in her own namespace the same call is accepted.
		if _, err := base.InNamespace("ns-a").PutNote(ctx, NoteInput{
			AuthorSession: "alice", AuthorID: "id-alice", Body: "back home", Addressee: "bob",
			ConversationID: "c-shared", MaxExchanges: maxExchanges,
		}, now); err != nil {
			t.Errorf("max_exchanges %d: threading in her own namespace = %v, want accepted", maxExchanges, err)
		}
	}
}

// TestNamespace_ExchangeBudgetCountsOnlyItsNamespace: the budget is a count of the
// thread's unexpired rows, and the same thread id in another namespace is a
// different thread. ns-a's five notes must not spend the default thread's budget.
func TestNamespace_ExchangeBudgetCountsOnlyItsNamespace(t *testing.T) {
	base, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()
	for i := range 5 {
		rawNote(t, base, "ns-a", "c-shared", "id-alice", fmt.Sprintf("ns-a note %d", i), now)
	}
	rawNote(t, base, "", "c-shared", "id-alice", "default 1", now)
	rawNote(t, base, "", "c-shared", "id-bob", "default 2", now)

	send := func(body string) error {
		_, err := base.PutNote(ctx, NoteInput{
			AuthorSession: "alice", AuthorID: "id-alice", Body: body, Addressee: "bob",
			ConversationID: "c-shared", MaxExchanges: 3,
		}, now)
		return err
	}
	if err := send("third in the default thread"); err != nil {
		t.Fatalf("the default thread holds 2 of 3 notes; the third must land (ns-a's 5 are another thread): %v", err)
	}
	if err := send("fourth"); !errors.Is(err, ErrConversationFull) {
		t.Fatalf("the default thread now holds 3 of 3; the fourth = %v, want ErrConversationFull (positive control)", err)
	}
}

// TestNamespace_ViewsShareOneProbeStatement: the namespace is a bound argument, not
// part of the SQL text, so every view probing the same shape reuses the one cached
// prepared statement instead of preparing a copy per namespace.
func TestNamespace_ViewsShareOneProbeStatement(t *testing.T) {
	base, _ := openTestStore(t)
	views := nsViews(base)
	ctx, now := context.Background(), time.Now()
	me := Claimant{Name: "alice", ID: "id-alice"}
	for _, ns := range nsAll {
		if _, err := views[ns].HasPendingNotes(ctx, me, now); err != nil {
			t.Fatal(err)
		}
	}
	base.probeMu.Lock()
	n := len(base.probeStmts)
	base.probeMu.Unlock()
	if n != 1 {
		t.Errorf("cached probe statements across %d namespaces = %d, want 1", len(nsAll), n)
	}
}
