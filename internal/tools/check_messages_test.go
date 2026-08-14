package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
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
		Workspace: func() string { return ws },
		PeerSessionByName: func(name string) (string, string, bool, bool) {
			return "sess-" + name, ws, true, false
		},
		PeerSessionByID: func(id string) (string, string, bool) {
			if name, ok := strings.CutPrefix(id, "id-"); ok {
				return name, ws, true
			}
			name, ok := strings.CutPrefix(id, "sess-")
			return name, ws, ok
		},
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
	targetID := ""
	if to != "" && to != collab.AddresseeNext {
		targetID = "sess-" + to
	}
	id, err := s.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: from, AuthorID: "id-" + from, Body: body, Addressee: to, TargetID: targetID,
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
	if !strings.Contains(out, "No notes") || !strings.Contains(out, "silence is not a refusal") {
		t.Errorf("wait timeout must report an empty result without implying refusal; got %q", out)
	}
}

// TestCheckMessages_WaitIsCappedByPolicy: a caller asking for an hour must not
// be able to park past the client's own call timeout.
func TestCheckMessages_WaitIsCappedByPolicy(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxWaitSeconds: 1}, "alice")
	start := time.Now()
	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{"wait_seconds":3600}`))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("wait ran for %s despite a 1s cap", elapsed)
	}
	if !strings.Contains(out, "waited 1s") ||
		!strings.Contains(out, "Requested wait 3600s was clamped") ||
		!strings.Contains(out, "max_wait_seconds=1s") {
		t.Errorf("wait result must report seconds and the clamp: %q", out)
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
	mine := Inbox{Self: "alice", SelfID: "id-alice", Root: "/proj/mine"}.Keys()
	theirs := Inbox{Self: "alice", SelfID: "id-alice", Root: "/proj/theirs"}.Keys()

	if mine[0] != "alice" {
		t.Errorf("a session is still addressed by name across projects; got %q", mine[0])
	}
	if mine[1] != collab.NotifySessionKey("id-alice") {
		t.Errorf("stable-ID wake key missing: %q", mine)
	}
	if mine[2] == collab.AddresseeNext {
		t.Error("the bare 'next' key is daemon-global — every project would share one wake-up")
	}
	if mine[2] == theirs[2] {
		t.Errorf("two workspaces must not share the 'next' wake-up key; both are %q", mine[2])
	}
}

// TestCheckMessages_UnreadableWakeDoesNotEndWaitOrDisclose proves a name-key
// bump outside this recipient's readable stores is neither a timing side channel
// nor a reason to violate the requested block-until-note-or-expiry contract.
func TestCheckMessages_UnreadableWakeDoesNotEndWaitOrDisclose(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxWaitSeconds: 1}, "alice")
	go func() {
		time.Sleep(50 * time.Millisecond)
		deps.Notifier.Bump("alice") // woken, but nothing this session may read
	}()

	start := time.Now()
	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{"wait_seconds":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("unreadable wake ended the wait after %s", elapsed)
	}
	if strings.Contains(out, "a message arrived") {
		t.Errorf("must not assert that a message exists for a session that could not read one; got %q", out)
	}
	if !strings.Contains(out, "No notes") {
		t.Errorf("expected an empty timeout result; got %q", out)
	}
}

// TestCheckMessages_BaselinesBeforeInitialClaim deterministically commits a note
// while the first claim is resolving its store. A post-claim baseline would absorb
// that bump and sleep through the pending row.
func TestCheckMessages_BaselinesBeforeInitialClaim(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxWaitSeconds: 2}, "alice")
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	deps.StoreIfExists = func() *collab.Store {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return nil
		}
		return local
	}
	writeErr := make(chan error, 1)
	go func() {
		<-entered
		_, err := local.PutNote(context.Background(), collab.NoteInput{
			AuthorSession: "bob", AuthorID: "id-bob", Body: "committed in the claim gap",
			Addressee: "alice", TargetID: deps.SessionID, TTL: time.Hour,
		}, time.Now())
		if err == nil {
			deps.Notifier.Bump("alice", collab.NotifySessionKey(deps.SessionID))
		}
		writeErr <- err
		close(release)
	}()

	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{"wait_seconds":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "committed in the claim gap") {
		t.Fatalf("wait slept through a note committed during its initial claim: %q", out)
	}
}

func TestCheckMessages_UnreadableWakeKeepsWaitingForReadableNote(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxWaitSeconds: 2}, "alice")
	writeErr := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		deps.Notifier.Bump("alice") // irrelevant wake
		time.Sleep(50 * time.Millisecond)
		_, err := local.PutNote(context.Background(), collab.NoteInput{
			AuthorSession: "bob", AuthorID: "id-bob", Body: "readable second wake",
			Addressee: "alice", TargetID: deps.SessionID, TTL: time.Hour,
		}, time.Now())
		if err == nil {
			deps.Notifier.Bump("alice", collab.NotifySessionKey(deps.SessionID))
		}
		writeErr <- err
	}()

	start := time.Now()
	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{"wait_seconds":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 75*time.Millisecond {
		t.Fatalf("irrelevant wake ended the wait after %s", elapsed)
	}
	if !strings.Contains(out, "readable second wake") {
		t.Fatalf("wait did not continue to the readable note: %q", out)
	}
}

// TestLeaveNote_OfflineThreadParticipantFailsClosed pins stable-identity
// routing: a disconnected participant cannot be replaced by a same-named live
// session or a historical workspace guess.
func TestLeaveNote_OfflineThreadParticipantFailsClosed(t *testing.T) {
	deps, local, global := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	myWS := wsRootOf(deps)
	conv := put(t, global, "bob", "alice", "question from my repo", "", "/proj/bob", myWS)
	if rows, err := global.ClaimNotesForSession(
		context.Background(), "alice", deps.SessionID, myWS, time.Now(), 1,
	); err != nil || len(rows) != 1 {
		t.Fatalf("establish alice's participation: rows=%#v err=%v", rows, err)
	}

	// An attacker can now reuse bob's display name, but not bob's stable ID.
	deps.PeerSessionByName = func(name string) (string, string, bool, bool) {
		if name == "bob" {
			return "id-attacker", "/proj/attacker", true, false
		}
		return "", "", false, false
	}
	deps.PeerSessionByID = func(string) (string, string, bool) { return "", "", false }

	_, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"bob","body":"my answer","conversation_id":`+jsonStr(conv)+`}`))
	if err == nil || !strings.Contains(err.Error(), "not active") ||
		!strings.Contains(err.Error(), "start a new conversation") {
		t.Fatalf("offline stable participant was not refused with a viable remedy: %v", err)
	}
	sent, err := global.RecentSentNotes(context.Background(), deps.SessionID, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 0 {
		t.Fatalf("refused reply created cross-project rows: %#v", sent)
	}
	if stray, err := local.PendingNotes(context.Background(), "bob", myWS, time.Now()); err != nil || len(stray) != 0 {
		t.Fatalf("refused reply leaked into the local name mailbox: rows=%#v err=%v", stray, err)
	}
}

func TestLeaveNote_ThreadReplyFollowsStableParticipantRename(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	ws := deps.Workspace()
	conv := put(t, local, "bob", "alice", "question", "", "", "")
	if rows, err := local.ClaimNotesForSession(
		context.Background(), "alice", deps.SessionID, ws, time.Now(), 1,
	); err != nil || len(rows) != 1 {
		t.Fatalf("establish alice's participation: rows=%#v err=%v", rows, err)
	}
	deps.PeerSessionByID = func(id string) (string, string, bool) {
		if id == "id-bob" {
			return "robert", ws, true
		}
		return "", "", false
	}
	// A stale/new session named bob must not influence the bound route.
	deps.PeerSessionByName = func(name string) (string, string, bool, bool) {
		if name == "bob" {
			return "id-attacker", "/proj/attacker", true, false
		}
		return "id-bob", ws, name == "robert", false
	}

	out, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"body":"answer","conversation_id":`+jsonStr(conv)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "session robert") {
		t.Fatalf("reply did not follow the stable participant's rename: %q", out)
	}
	rows, err := local.ClaimNotesForSession(context.Background(), "robert", "id-bob", ws, time.Now(), 1)
	if err != nil || len(rows) != 1 || rows[0].Body != "answer" {
		t.Fatalf("renamed participant did not receive reply: rows=%#v err=%v", rows, err)
	}
}

// TestLeaveNote_UnplaceablePeerSaysSo: when nothing can place the name, the
// message is still filed locally — addressing a peer that has not attached yet
// is legitimate — but the sender is told, so "sent" is never mistaken for
// "delivered to the agent I meant".
func TestLeaveNote_UnplaceablePeerSaysSo(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	deps.PeerSessionByName = func(string) (string, string, bool, bool) {
		return "", "", false, false
	}

	out, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"ghost","body":"hello?"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unplaced") || !strings.Contains(out, "THIS workspace") {
		t.Errorf("an unplaceable recipient must be flagged to the sender; got %q", out)
	}
}

// TestCheckMessages_NeverClaimsSelfAuthoredNotes pins the final delivery guard:
// an address collision may target this display name, but this session cannot
// consume its own authored row.
func TestCheckMessages_NeverClaimsSelfAuthoredNotes(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	if _, err := local.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "alice",
		AuthorID:      deps.SessionID,
		Body:          "do not loop",
		Addressee:     "alice",
		TTL:           time.Hour,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No notes") || strings.Contains(out, "do not loop") {
		t.Fatalf("self-authored note was delivered: %q", out)
	}
	pending, err := local.PendingNotes(context.Background(), "alice", deps.Workspace(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != "do not loop" {
		t.Fatalf("self-authored note was consumed instead of left for another eligible session: %#v", pending)
	}
}
