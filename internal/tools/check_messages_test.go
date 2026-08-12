package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// chatTestDeps builds a session wired to its own workspace store plus a
// SEPARATE store standing in for the daemon-level cross-project one, so a test
// can prove the recipient's cross_project flag is what gates the second.
func chatTestDeps(t *testing.T, policy CollabPolicy, self string) (CollabDeps, *collab.Store, *collab.Store) {
	t.Helper()
	ws := t.TempDir()
	local, err := collab.Open(ws)
	if err != nil {
		t.Fatalf("open workspace store: %v", err)
	}
	// A REAL daemon-level store (empty workspace ⇒ IsGlobal), not a second
	// workspace store — otherwise the target-workspace addressing rules that
	// protect cross-project mail would never be exercised.
	global, err := collab.OpenGlobalAt(filepath.Join(t.TempDir(), "collab-xproject.db"))
	if err != nil {
		t.Fatalf("open cross-project store: %v", err)
	}
	t.Cleanup(func() { _ = local.Close(); _ = global.Close() })

	deps := CollabDeps{
		Workspace:           func() string { return ws },
		ResolvePeer:         func(string) (PeerSession, bool) { return PeerSession{}, false },
		SessionName:         func() string { return self },
		SessionID:           "sess-" + self,
		Policy:              func() CollabPolicy { return policy },
		Store:               func() *collab.Store { return local },
		StoreIfExists:       func() *collab.Store { return local },
		GlobalStore:         func() *collab.Store { return global },
		GlobalStoreIfExists: func() *collab.Store { return global },
		Notifier:            collab.NewNotifier(),
	}
	return deps, local, global
}

// wsRootOf reports the workspace a chatTestDeps session is pinned to.
func wsRootOf(d CollabDeps) string { return d.Workspace() }

func put(t *testing.T, s *collab.Store, from, to, body, conv, origin, target string) string {
	t.Helper()
	id, err := s.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: from, AuthorID: "id-" + from, Body: body, Addressee: to,
		TTL: time.Hour, ConversationID: conv, OriginWorkspace: origin, TargetWorkspace: target,
	}, time.Now())
	if err != nil {
		t.Fatalf("PutNote: %v", err)
	}
	return id
}

func TestCheckMessages_DisabledRefusesCleanly(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: false}, "alice")
	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("disabled should not error: %v", err)
	}
	if !strings.Contains(out, "disabled") || !strings.Contains(out, "mailbox = true") {
		t.Errorf("expected a clear enable hint; got %q", out)
	}
}

// TestCheckMessages_DeliversOnceWithConversationID: reading hands the message
// over with the id needed to reply, and a second read does not repeat it.
func TestCheckMessages_DeliversOnceWithConversationID(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	conv := put(t, local, "bob", "alice", "can you take the parser?", "", "", "")

	tool := NewCheckMessages(deps)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "can you take the parser?") {
		t.Errorf("message body missing from delivery: %q", out)
	}
	if !strings.Contains(out, conv) {
		t.Errorf("delivery must quote the conversation id %q so a reply can thread; got %q", conv, out)
	}
	if !strings.Contains(out, "bob") {
		t.Errorf("delivery must name the sender; got %q", out)
	}

	again, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(again, "can you take the parser?") {
		t.Errorf("a delivered message must not be handed over twice; got %q", again)
	}
}

// TestCheckMessages_CrossProjectGatedByRecipient is the consent rule: the
// SENDER never decides. A message sitting in the daemon-level store is invisible
// until the recipient's own project opts in.
func TestCheckMessages_CrossProjectGatedByRecipient(t *testing.T) {
	off := CollabPolicy{Mailbox: true, CrossProject: false}
	deps, _, global := chatTestDeps(t, off, "alice")
	put(t, global, "carol", "alice", "ping from another repo", "", "/other/project", wsRootOf(deps))

	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "ping from another repo") {
		t.Fatal("a cross-project message must NOT be delivered while cross_project is off")
	}

	// Same stores, same unread row — only the recipient's flag changes.
	on := CollabPolicy{Mailbox: true, CrossProject: true}
	deps.Policy = func() CollabPolicy { return on }
	out, err = NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ping from another repo") {
		t.Fatalf("cross_project on should deliver it; got %q", out)
	}
	if !strings.Contains(out, "/other/project") {
		t.Errorf("a cross-project message must be labelled with its origin project; got %q", out)
	}
}

// TestCheckMessages_WaitTimesOutAndExplainsSilence: an expired wait must not
// leave the agent believing the peer refused.
func TestCheckMessages_WaitTimesOutAndExplains(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxWaitSeconds: 1}, "alice")
	start := time.Now()
	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{"wait_seconds":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("wait returned after %s — it did not actually block", elapsed)
	}
	if !strings.Contains(out, "No messages") {
		t.Errorf("expected an empty result; got %q", out)
	}
}

// TestCheckMessages_WaitIsCappedByPolicy: a caller asking for an hour must not
// be able to park past the client's own call timeout.
func TestCheckMessages_WaitIsCappedByPolicy(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxWaitSeconds: 1}, "alice")
	start := time.Now()
	if _, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{"wait_seconds":3600}`)); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("wait ran for %s despite a 1s cap", elapsed)
	}
}

// TestCheckMessages_WaitWakesOnDelivery is the turn-taking path end to end: a
// parked session is released by a peer's send and reads it in the same call.
func TestCheckMessages_WaitWakesOnDelivery(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxWaitSeconds: 5}, "alice")

	go func() {
		time.Sleep(50 * time.Millisecond)
		put(t, local, "bob", "alice", "here is the answer", "", "", "")
		deps.Notifier.Bump("alice")
	}()

	start := time.Now()
	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{"wait_seconds":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "here is the answer") {
		t.Fatalf("a message sent during the wait must be delivered; got %q", out)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("woke after %s — it slept through the bump instead of being signalled", elapsed)
	}
}

// TestInboxKeys_NextIsScopedToWorkspace: the "next arrival" wake-up key must
// differ per workspace. Watching the bare "next" literal made every connection in
// the daemon — including sessions pinned to unrelated projects — fail its
// "nothing new" check and run a needless claim whenever anyone, anywhere, left a
// note for the next arrival.
func TestInboxKeys_NextIsScopedToWorkspace(t *testing.T) {
	mine := Inbox{Self: "alice", Root: "/proj/mine"}.Keys()
	theirs := Inbox{Self: "alice", Root: "/proj/theirs"}.Keys()

	if mine[0] != "alice" {
		t.Errorf("a session is still addressed by name across projects; got %q", mine[0])
	}
	if mine[1] == collab.AddresseeNext {
		t.Error("the bare 'next' key is daemon-global — every project would share one wake-up")
	}
	if mine[1] == theirs[1] {
		t.Errorf("two workspaces must not share the 'next' wake-up key; both are %q", mine[1])
	}
}

// TestCheckMessages_WokenWithNothingReadableDisclosesNothing: a wake-up is not
// evidence that a message for THIS session exists — a name is a daemon-wide
// notifier key, so a send to a same-named peer in a project this one cannot read
// wakes it too. Claiming "a message arrived" would then be both false and a
// disclosure of something the agent is not allowed to see.
func TestCheckMessages_WokenWithNothingReadableDisclosesNothing(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxWaitSeconds: 5}, "alice")
	go func() {
		time.Sleep(50 * time.Millisecond)
		deps.Notifier.Bump("alice") // woken, but nothing this session may read
	}()

	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{"wait_seconds":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "a message arrived") {
		t.Errorf("must not assert that a message exists for a session that could not read one; got %q", out)
	}
	if !strings.Contains(out, "No messages") {
		t.Errorf("expected an empty result; got %q", out)
	}
}

// TestLeaveNote_OfflinePeerIsPlacedByConversation pins the routing fix. Routing
// used to ask only "is a session with that name live right now"; a peer that
// exited between turns of a cross-project conversation made the next reply fall
// back to the SENDER's own mailbox — invisible to the recipient, reported as
// sent, and with the exchange budget silently reset. The conversation itself
// records where the peer was writing from, so it can still be placed.
func TestLeaveNote_OfflinePeerIsPlacedByConversation(t *testing.T) {
	deps, local, global := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	myWS := wsRootOf(deps)

	// bob wrote to alice from another project, then disconnected.
	conv := put(t, global, "bob", "alice", "question from my repo", "", "/proj/bob", myWS)
	deps.ResolvePeer = func(string) (PeerSession, bool) { return PeerSession{}, false } // bob is gone

	out, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"bob","body":"my answer","conversation_id":`+jsonStr(conv)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cross-project") {
		t.Errorf("the reply should still be routed cross-project; got %q", out)
	}

	// It must be in the cross-project store addressed back to bob's workspace —
	// NOT in alice's own mailbox where bob could never see it.
	back, err := global.ClaimNotes(context.Background(), collab.Claimant{Name: "bob", Workspace: "/proj/bob"}, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0].Body != "my answer" {
		t.Fatalf("bob should be able to claim the reply in his workspace; got %v", back)
	}
	stray, err := local.ClaimNotes(context.Background(), collab.Claimant{Name: "bob", Workspace: myWS}, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stray) != 0 {
		t.Errorf("the reply must not be filed in the sender's own mailbox; found %d", len(stray))
	}
}

// TestLeaveNote_UnplaceablePeerSaysSo: when nothing can place the name, the
// message is still filed locally — addressing a peer that has not attached yet
// is legitimate — but the sender is told, so "sent" is never mistaken for
// "delivered to the agent I meant".
func TestLeaveNote_UnplaceablePeerSaysSo(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	deps.ResolvePeer = func(string) (PeerSession, bool) { return PeerSession{}, false }

	out, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"ghost","body":"hello?"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unplaced") || !strings.Contains(out, "THIS workspace") {
		t.Errorf("an unplaceable recipient must be flagged to the sender; got %q", out)
	}
}

// TestLeaveNote_ExchangeBudgetRefusesReply: the backstop against two agents
// answering each other indefinitely. Opening a thread is always allowed; it is
// the reply into a spent thread that is refused, with an instruction to involve
// the human rather than to start a fresh thread.
func TestLeaveNote_ExchangeBudgetRefusesReply(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxExchanges: 3}, "alice")

	conv := put(t, local, "bob", "alice", "1", "", "", "")
	put(t, local, "alice", "bob", "2", conv, "", "")
	put(t, local, "bob", "alice", "3", conv, "", "")

	out, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"bob","body":"4","conversation_id":`+jsonStr(conv)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "limit") || !strings.Contains(out, "NOT sent") {
		t.Errorf("expected a clear budget refusal; got %q", out)
	}
	if !strings.Contains(out, "human") {
		t.Errorf("the refusal must point the agent at its human; got %q", out)
	}
	if n, err := local.ConversationCount(context.Background(), conv); err != nil || n != 3 {
		t.Errorf("the refused reply must not be stored: count=%d err=%v", n, err)
	}

	// A NEW conversation is unaffected — the budget is per thread.
	fresh, err := NewLeaveNote(deps).Execute(context.Background(), json.RawMessage(`{"to":"bob","body":"new topic"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fresh, "NOT sent") {
		t.Errorf("a fresh conversation must not inherit a spent budget; got %q", fresh)
	}
}

// TestCheckMessages_UnregisteredSessionHasNoAddress. A session whose
// registration failed keeps a display name for the TUI and logs, but that name
// entered no file any peer's uniqueness check can see, so it may duplicate a
// live session's. Claiming is destructive — a message is handed over exactly
// once — so an addressable shadow silently swallows the real recipient's mail.
// Every other delivery path takes its address from connSession.inbox, which
// withholds one here; check_messages built its own from the display name and was
// the single lane around that gate.
func TestCheckMessages_UnregisteredSessionHasNoAddress(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	deps.SessionID = "" // session.Register failed
	put(t, local, "bob", "alice", "meant for the real alice", "", "", "")

	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "meant for the real alice") {
		t.Fatalf("an unregistered session claimed a live peer's message: %q", out)
	}
	if !strings.Contains(out, "not registered") {
		t.Errorf("the refusal should say why there is no address; got %q", out)
	}

	// And the message is still there for whoever legitimately holds the name.
	got, err := local.ClaimNotes(context.Background(),
		collab.Claimant{Name: "alice", ID: "sess-alice"}, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the real recipient claimed %d messages, want 1 — the shadow consumed it", len(got))
	}
}

// TestCheckMessages_DeliversABoundMessageToItsOwnSession pins the wiring, not
// the predicate: the inbox must present this session's ID as well as its name.
// Dropping it fails in the quiet direction — bound messages simply never arrive,
// for anyone — which no test of the impersonation case would ever catch.
func TestCheckMessages_DeliversABoundMessageToItsOwnSession(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	if _, err := local.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "bob", AuthorID: "id-bob", Body: "bound and addressed to me",
		Addressee: "alice", AddresseeID: deps.SessionID, TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bound and addressed to me") {
		t.Fatalf("a message bound to this very session must be delivered; got %q", out)
	}
}
