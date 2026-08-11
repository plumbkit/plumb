package tools

import (
	"context"
	"encoding/json"
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
	ws, xws := t.TempDir(), t.TempDir()
	local, err := collab.Open(ws)
	if err != nil {
		t.Fatalf("open workspace store: %v", err)
	}
	global, err := collab.Open(xws)
	if err != nil {
		t.Fatalf("open cross-project store: %v", err)
	}
	t.Cleanup(func() { _ = local.Close(); _ = global.Close() })

	deps := CollabDeps{
		Workspace:           func() string { return ws },
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

func put(t *testing.T, s *collab.Store, from, to, body, conv, origin string) string {
	t.Helper()
	id, err := s.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: from, AuthorID: "id-" + from, Body: body, Addressee: to,
		TTL: time.Hour, ConversationID: conv, OriginWorkspace: origin,
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
	conv := put(t, local, "bob", "alice", "can you take the parser?", "", "")

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
	put(t, global, "carol", "alice", "ping from another repo", "", "/other/project")

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
		put(t, local, "bob", "alice", "here is the answer", "", "")
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

// TestLeaveNote_ExchangeBudgetRefusesReply: the backstop against two agents
// answering each other indefinitely. Opening a thread is always allowed; it is
// the reply into a spent thread that is refused, with an instruction to involve
// the human rather than to start a fresh thread.
func TestLeaveNote_ExchangeBudgetRefusesReply(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxExchanges: 3}, "alice")

	conv := put(t, local, "bob", "alice", "1", "", "")
	put(t, local, "alice", "bob", "2", conv, "")
	put(t, local, "bob", "alice", "3", conv, "")

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
