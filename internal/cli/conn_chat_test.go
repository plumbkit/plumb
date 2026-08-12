package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
)

// newChatTestSession builds a connSession wired to a real per-temp-workspace
// collab pool with a display name, so the message-delivery path can be
// exercised hermetically.
func newChatTestSession(t *testing.T, ws, name string, cc config.CollabConfig) *connSession {
	t.Helper()
	s := &connSession{
		store:      config.NewStore(config.Defaults()),
		collabPool: newCollabPool(),
		chatWatch:  &chatWatch{},
		hintCache:  &memoryHintCache{},
		peerWrites: &peerWriteCache{},
		sessID:     "self",
		ctx:        context.Background(),
	}
	s.mutate(func(v *sessionView) {
		v.acquiredRoot = ws
		v.collab = cc
		v.sessName = name
	})
	t.Cleanup(func() { s.collabPool.closeAll() })
	return s
}

// seedMessage stores a message addressed to `to` in the workspace store.
func seedMessage(t *testing.T, s *connSession, ws, from, to, body string) {
	t.Helper()
	store := s.collabPool.acquire(ws)
	if store == nil {
		t.Fatal("acquire collab store")
	}
	if _, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: from, AuthorID: "peer", Body: body, Addressee: to, TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.collabPool.notifier().Bump(collab.NotifyKey(ws, to))
}

// TestMessageHint_NextNoteElsewhereDoesNotForceAQuery is the cost guarantee
// across projects. One daemon hosts every workspace's connections, so while the
// "next arrival" address shared a single notifier key, a note left for the next
// arrival in ANY repository invalidated every connection's cached baseline and
// made each of them claim against its own store to learn the note was not theirs.
func TestMessageHint_NextNoteElsewhereDoesNotForceAQuery(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	notifier := s.collabPool.notifier()
	keys := s.inbox().Keys()
	now := time.Now()
	s.chatWatch.due(keys, notifier.Gens(keys), now) // establish the baseline

	// A peer in an unrelated project leaves a note for whoever attaches THERE next.
	notifier.Bump(collab.NotifyKey(t.TempDir(), collab.AddresseeNext))

	if s.chatWatch.due(keys, notifier.Gens(keys), now.Add(time.Second)) {
		t.Error("a 'next' note in another workspace must not make this connection query its store")
	}
	// The same note left HERE must still wake us, or the scoping has gone too far.
	notifier.Bump(collab.NotifyKey(ws, collab.AddresseeNext))
	if !s.chatWatch.due(keys, notifier.Gens(keys), now.Add(2*time.Second)) {
		t.Error("a 'next' note in this workspace must still trigger a query")
	}
}

// TestMessageHint_DeliversOnAnyToolNotJustPathBearing is the behavioural point
// of piggybacking on the enrich hook: a message is about the agent, not about
// the file it happens to be touching, so an agent working through git alone
// must still receive it.
func TestMessageHint_DeliversOnAnyToolNotJustPathBearing(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	seedMessage(t, s, ws, "bob", "alice", "the parser branch is yours")

	// "git" is not in hintAllowedTools — under the old gating it would get nothing.
	got := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "On branch main\n")
	if !strings.Contains(got, "the parser branch is yours") {
		t.Fatalf("a message must be delivered on a non-path-bearing tool; got %q", got)
	}
	if !strings.Contains(got, "On branch main") {
		t.Error("the tool's own output must be preserved")
	}
}

// TestMessageHint_DisabledStaysSilent: the mailbox flag gates delivery, and the
// off path must not touch the store at all.
func TestMessageHint_DisabledStaysSilent(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: false})
	if got := s.messageHint(context.Background()); got != "" {
		t.Errorf("mailbox off must deliver nothing; got %q", got)
	}
	if collab.Exists(ws) {
		t.Error("the disabled delivery path must not create a collab.db")
	}
}

// TestMessageHint_NoWorkspaceStoreIsSilent: a workspace that has never used the
// feature must not gain a collab.db just because a tool call happened.
func TestMessageHint_NoStoreDoesNotCreateOne(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true})
	if got := s.messageHint(context.Background()); got != "" {
		t.Errorf("no messages should render nothing; got %q", got)
	}
	if collab.Exists(ws) {
		t.Error("delivery must never create a collab.db — only a send may")
	}
}

// TestMessageHint_DeliveredOnceAcrossCalls: the watermark holds on the hot path
// too, so a message does not repeat on every subsequent tool call.
func TestMessageHint_DeliveredOnceAcrossCalls(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	seedMessage(t, s, ws, "bob", "alice", "only once please")

	first := s.messageHint(context.Background())
	if !strings.Contains(first, "only once please") {
		t.Fatalf("first call should deliver; got %q", first)
	}
	if second := s.messageHint(context.Background()); second != "" {
		t.Errorf("second call must be silent; got %q", second)
	}
}

// TestMessageHint_SilentForMailboxTools: each of these surfaces the same
// messages itself — check_messages claims and renders, session_start renders its
// "## Messages" section, workspace_sessions lists the unread ones — so
// piggybacking would show them twice in one response.
func TestMessageHint_SilentForMailboxTools(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	seedMessage(t, s, ws, "bob", "alice", "should not double-deliver")

	for _, tool := range []string{"check_messages", "session_start", "workspace_sessions"} {
		got := s.enrichToolOutput(context.Background(), tool, json.RawMessage(`{}`), "out")
		if strings.Contains(got, "should not double-deliver") {
			t.Errorf("%s must not carry a piggybacked message block", tool)
		}
	}
}

// TestMessageHint_DeliveredOnALeaveNoteResult is the fan-out case that leaving
// leave_note in mailboxSilentTools lost outright.
//
// leave_note surfaces nothing of its own — no Inbox, no claim, no
// RenderMessages — so it cannot double-deliver; the only effect of its silence
// was to skip delivery on the call an exchange most often makes next. An agent
// messaging alice, then bob, then carol never renders alice's reply at all: each
// leave_note suppresses it and the next call is another leave_note.
func TestMessageHint_DeliveredOnALeaveNoteResult(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	seedMessage(t, s, ws, "bob", "alice", "the parser branch is yours")

	got := s.enrichToolOutput(context.Background(), "leave_note",
		json.RawMessage(`{}`), "Message sent to session carol.\n")
	if !strings.Contains(got, "the parser branch is yours") {
		t.Errorf("a pending reply must ride back on a leave_note result; got %q", got)
	}
	if !strings.Contains(got, "Message sent to session carol.") {
		t.Errorf("the tool's own result must survive enrichment; got %q", got)
	}
	// Still exactly once: the claim is shared, so the next call gets nothing.
	if again := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "OUT"); strings.Contains(again, "the parser branch is yours") {
		t.Errorf("the message was delivered twice; got %q", again)
	}
}

// TestChatWatch_SkipsQueryWhenNothingChanged is the cost guarantee. The enrich
// hook runs on EVERY tool call, so the steady state — no mail — must not reach
// the database. `due` is the gate that decides, and it must say no while the
// generations are unchanged and the periodic backstop is not yet due.
func TestChatWatch_SkipsQueryWhenNothingChanged(t *testing.T) {
	w := &chatWatch{}
	keys := []string{"alice", collab.AddresseeNext}
	gens := []uint64{0, 0}
	now := time.Now()

	if !w.due(keys, gens, now) {
		t.Fatal("the first call must consult the store — nothing is cached yet")
	}
	if w.due(keys, gens, now.Add(time.Second)) {
		t.Error("unchanged generations inside the backstop window must NOT trigger a query")
	}
	if !w.due(keys, []uint64{1, 0}, now.Add(2*time.Second)) {
		t.Error("an advanced generation must trigger a query")
	}
	if w.due(keys, []uint64{1, 0}, now.Add(3*time.Second)) {
		t.Error("the advanced generation is now the cached baseline; a repeat must be skipped")
	}
}

// TestChatWatch_BackstopFiresAfterInterval: the counters are a fast path, not
// the truth. They reset when the daemon restarts, so a periodic full check must
// still happen or a message written by a previous daemon would never arrive.
func TestChatWatch_BackstopFiresAfterInterval(t *testing.T) {
	w := &chatWatch{}
	keys := []string{"alice"}
	gens := []uint64{7}
	now := time.Now()

	w.due(keys, gens, now)
	if w.due(keys, gens, now.Add(chatFullCheckInterval-time.Second)) {
		t.Error("before the interval elapses, an unchanged generation must skip the query")
	}
	if !w.due(keys, gens, now.Add(chatFullCheckInterval+time.Second)) {
		t.Error("the periodic backstop must fire even with unchanged generations")
	}
}

// TestChatWatch_ResetForcesRecheck: a re-pin points the session at a different
// project's collab.db, about which the old counters say nothing.
func TestChatWatch_ResetForcesRecheck(t *testing.T) {
	w := &chatWatch{}
	keys := []string{"alice"}
	gens := []uint64{3}
	now := time.Now()

	w.due(keys, gens, now)
	if w.due(keys, gens, now.Add(time.Second)) {
		t.Fatal("precondition: an unchanged generation should be skipped")
	}
	w.reset()
	if !w.due(keys, gens, now.Add(2*time.Second)) {
		t.Error("after a re-pin the store must be consulted again")
	}
}

// TestEnrichOrder_MessagesComeAfterPathHints pins the ordering, which is a
// correctness property rather than a cosmetic one. Claiming a message marks it
// delivered for good, and runHookSafely discards the entire enriched string if a
// later step panics — so the mutating step must run after the read-only ones. If
// delivery moved back ahead of the hints, a panic in a hint would destroy a
// message that can never be offered again.
func TestEnrichOrder_MessagesComeAfterPathHints(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	s.applyProjectConfig(ws)
	writePathMemory(t, ws, "auth-gotchas", "internal/auth/**")
	seedMessage(t, s, ws, "bob", "alice", "message body here")

	args := json.RawMessage(`{"file_path":"` + filepath.Join(ws, "internal/auth/token.go") + `"}`)
	got := s.enrichToolOutput(context.Background(), "read_file", args, "FILE OUTPUT")

	hintAt := strings.Index(got, "auth-gotchas")
	msgAt := strings.Index(got, "message body here")
	if hintAt < 0 {
		t.Fatalf("precondition: the memory hint should have fired; got %q", got)
	}
	if msgAt < 0 {
		t.Fatalf("precondition: the message should have been delivered; got %q", got)
	}
	if msgAt < hintAt {
		t.Errorf("messages must be appended AFTER the path hints, so a panic in a hint "+
			"cannot destroy an already-claimed message; got:\n%s", got)
	}
}

// panicHintSession returns a session whose path hints panic, standing in for any
// bug in the read-only hint path — the reason runHookSafely exists at all.
func panicHintSession(t *testing.T, ws, name string, cc config.CollabConfig) *connSession {
	t.Helper()
	s := newChatTestSession(t, ws, name, cc)
	s.applyProjectConfig(ws) // turns memory hint injection on, so the path runs
	s.hintCache = nil        // memoryHint dereferences this, so the hint path panics
	return s
}

// TestEnrichPanic_InAPathHintDoesNotConsumeAMessage pins the property the
// ordering exists for, and which an earlier attempt at this fix got wrong.
//
// Claiming marks a row delivered irreversibly, while runHookSafely discards the
// entire enriched string when the hook panics. So if delivery can run while the
// stack is unwinding, the message is claimed and then thrown away — lost for
// good. A `defer` would do exactly that, because deferred calls run DURING panic
// unwinding; delivery must therefore be an ordinary statement that the panic
// skips. This test fails if anyone reintroduces the defer.
func TestEnrichPanic_InAPathHintDoesNotConsumeAMessage(t *testing.T) {
	ws := t.TempDir()
	s := panicHintSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	writePathMemory(t, ws, "auth-gotchas", "**")
	seedMessage(t, s, ws, "bob", "alice", "must survive the panic")

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("precondition: the path hint was expected to panic")
			}
		}()
		args := json.RawMessage(`{"file_path":"` + filepath.Join(ws, "a.go") + `"}`)
		_ = s.enrichToolOutput(context.Background(), "read_file", args, "OUT")
	}()

	// The enriched text was discarded by the panic, so the message must NOT have
	// been claimed — it has to still be waiting for the next call.
	store := s.collabPool.get(ws)
	if store == nil {
		t.Fatal("expected the seeded store to exist")
	}
	pending, err := store.PendingNotes(context.Background(), collab.Claimant{Name: "alice", Workspace: ws}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("a message must not be consumed by a hook that panicked and discarded "+
			"its output; pending = %d, want 1", len(pending))
	}
}

// seedBoundMessage stores a message addressed to `to` AND bound to one session
// id, so only that session may read it.
func seedBoundMessage(t *testing.T, s *connSession, ws, to, addresseeID, body string) {
	t.Helper()
	store := s.collabPool.acquire(ws)
	if store == nil {
		t.Fatal("acquire collab store")
	}
	if _, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "bob", AuthorID: "peer", Body: body, Addressee: to,
		AddresseeID: addresseeID, TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.collabPool.notifier().Bump(collab.NotifyKey(ws, to))
}

// TestMessageHint_BoundMessageReachesOnlyItsOwnSession pins the piggyback lane's
// half of the binding, in both directions. This connection's inbox must carry
// its session ID: without it a bound message is delivered to nobody, and with
// the wrong one it would be delivered to a session that merely shares the name.
func TestMessageHint_BoundMessageReachesOnlyItsOwnSession(t *testing.T) {
	cc := config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512}

	ws := t.TempDir()
	mine := newChatTestSession(t, ws, "alice", cc)
	seedBoundMessage(t, mine, ws, "alice", mine.sessID, "for this alice only")
	if got := mine.messageHint(context.Background()); !strings.Contains(got, "for this alice only") {
		t.Fatalf("the session a message is bound to must receive it; got %q", got)
	}

	// A different workspace, so the fresh session is racing nothing but the name.
	otherWS := t.TempDir()
	successor := newChatTestSession(t, otherWS, "alice", cc)
	seedBoundMessage(t, successor, otherWS, "alice", "sess-that-has-ended", "for the previous alice")
	if got := successor.messageHint(context.Background()); strings.Contains(got, "for the previous alice") {
		t.Fatalf("a session reusing the name read its predecessor's message; got %q", got)
	}
}
