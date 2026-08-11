package cli

import (
	"context"
	"encoding/json"
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
	s.collabPool.notifier().Bump(to)
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

// TestMessageHint_SilentForMailboxTools: check_messages and leave_note claim and
// render messages themselves, so piggybacking on them would double-deliver.
func TestMessageHint_SilentForMailboxTools(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	seedMessage(t, s, ws, "bob", "alice", "should not double-deliver")

	for _, tool := range []string{"check_messages", "leave_note", "session_start"} {
		got := s.enrichToolOutput(context.Background(), tool, json.RawMessage(`{}`), "out")
		if strings.Contains(got, "should not double-deliver") {
			t.Errorf("%s must not carry a piggybacked message block", tool)
		}
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
