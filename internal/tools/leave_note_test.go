package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

func TestLeaveNote_DisabledRefusesCleanly(t *testing.T) {
	deps, _, created := collabTestDeps(t, CollabPolicy{Mailbox: false})
	tool := NewLeaveNote(deps)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"body":"hi"}`))
	if err != nil {
		t.Fatalf("disabled should not error: %v", err)
	}
	if !strings.Contains(out, "disabled") || !strings.Contains(out, "mailbox = true") {
		t.Errorf("expected a clear enable hint; got %q", out)
	}
	if *created {
		t.Error("the disabled path must not touch the collab store")
	}
}

func TestLeaveNote_DefaultsToNext(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	tool := NewLeaveNote(deps)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"body":"welcome"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "next session") {
		t.Errorf("expected next-arrival wording; got %q", out)
	}
	got, err := store.ClaimNotes(context.Background(), collab.Claimant{Name: "whoever"}, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Addressee != collab.AddresseeNext {
		t.Fatalf("note should default to the 'next' addressee; got %v", got)
	}
}

func TestLeaveNote_AddressedAndRedacted(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	tool := NewLeaveNote(deps)
	body := `heads up token=abcdef0123456789ghijkl`
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"body":`+jsonStr(body)+`,"to":"alice"}`))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingNotes(context.Background(), collab.Claimant{Name: "alice"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending for alice = %d, want 1", len(pending))
	}
	if strings.Contains(pending[0].Body, "abcdef0123456789") {
		t.Errorf("note body persisted UNREDACTED: %q", pending[0].Body)
	}
}

// TestLeaveNote_BindsToTheLivePeerButNotToAnAbsentOne is the send half of the
// impersonation fix. A message can only be bound to a session that exists to be
// bound to, so the tool stamps the peer's session id when one is live and stamps
// nothing when the name resolves to no live session — the latter being the case
// that must keep working, since addressing a peer that is not connected is
// legitimate and always was.
func TestLeaveNote_BindsToTheLivePeerButNotToAnAbsentOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		peer   PeerSession
		live   bool
		wantID string
	}{
		{"live peer is bound", PeerSession{Workspace: "", ID: "sess-alice"}, true, "sess-alice"},
		{"absent peer stays unbound", PeerSession{}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
			deps.ResolvePeer = func(string) (PeerSession, bool) { return tc.peer, tc.live }

			if _, err := NewLeaveNote(deps).Execute(context.Background(),
				json.RawMessage(`{"to":"alice","body":"ping"}`)); err != nil {
				t.Fatal(err)
			}
			pending, err := store.PendingNotes(context.Background(),
				collab.Claimant{Name: "alice", ID: tc.wantID}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 1 {
				t.Fatalf("pending for the intended session = %d, want 1", len(pending))
			}
			if got := pending[0].AddresseeID; got != tc.wantID {
				t.Fatalf("stored AddresseeID = %q, want %q", got, tc.wantID)
			}
		})
	}
}

// TestLeaveNote_BoundMessageIsUnreadableByANameReuser walks the whole path the
// hole ran through: send to a live peer, the peer's session ends, a new session
// takes the name. The successor must read nothing, and the sender must have been
// told the message was bound rather than merely sent.
func TestLeaveNote_BoundMessageIsUnreadableByANameReuser(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	deps.ResolvePeer = func(string) (PeerSession, bool) { return PeerSession{ID: "sess-alice-1"}, true }

	out, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"alice","body":"rotate the key before Friday"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bound to") {
		t.Errorf("the sender must be told delivery is bound to that live session; got %q", out)
	}

	// alice-1 exits without reading. alice-2 arrives under the same name.
	got, err := store.ClaimNotes(context.Background(),
		collab.Claimant{Name: "alice", ID: "sess-alice-2"}, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a successor session read %d message(s) meant for its predecessor", len(got))
	}
}

// TestLeaveNote_TruncationWarnsSenderAndWithholdsFullBody is PLAN-301 D1's
// send half. A body over chat_budget_bytes must not be echoed back whole
// under a success line — that echo is what made the eventual delivery-time
// cut invisible to the sender.
func TestLeaveNote_TruncationWarnsSenderAndWithholdsFullBody(t *testing.T) {
	deps, _, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120, ChatBudgetBytes: 20})
	tool := NewLeaveNote(deps)
	body := strings.Repeat("x", 100)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"body":`+jsonStr(body)+`,"to":"alice"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TRUNCATED") {
		t.Errorf("expected a truncation warning; got %q", out)
	}
	if strings.Contains(out, body) {
		t.Errorf("the full over-budget body must not be echoed back; got %q", out)
	}
	if !strings.Contains(out, "conversation_id") {
		t.Errorf("expected the remedy to name the conversation_id resend; got %q", out)
	}
	// Three of the twenty budgeted bytes go on the trim marker, so seventeen of
	// the hundred arrive and eighty-three do not. Quoting twenty and eighty here
	// would be the same silent loss the warning exists to end: a sender resending
	// "the remaining 80" would start at byte 20 and drop three bytes for good.
	if !strings.Contains(out, "only 17 of 100 bytes") {
		t.Errorf("expected the delivered count to exclude the trim marker; got %q", out)
	}
	if !strings.Contains(out, "remaining 83 bytes (everything from byte 17 on)") {
		t.Errorf("expected the remedy to name the exact resend offset; got %q", out)
	}
}

// TestLeaveNote_ReportsByteBudgetOnSuccess is PLAN-301 D3: a send under
// budget should tell the sender the shape of the limit before they lose
// content to it, not just silently succeed.
func TestLeaveNote_ReportsByteBudgetOnSuccess(t *testing.T) {
	deps, _, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120, ChatBudgetBytes: 2048})
	tool := NewLeaveNote(deps)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"body":"hello","to":"alice"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "5 of 2048") {
		t.Errorf("expected the byte budget reported as \"5 of 2048\"; got %q", out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("a body under budget should still be echoed; got %q", out)
	}
}

// TestLeaveNote_RefusesCrossProjectWhenTargetHasNotOptedIn is PLAN-334: a note
// addressed to a live peer pinned to a DIFFERENT workspace must not be silently
// accepted and reported sent when that project has not opted in to
// [collab] cross_project — it would sit unclaimed until it expires, with the
// sender never told. The refusal must be explicit, and nothing may be written
// to the cross-project store.
func TestLeaveNote_RefusesCrossProjectWhenTargetHasNotOptedIn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		allows func(string) bool
	}{
		{"target explicitly declines", func(string) bool { return false }},
		{"consent unwired — cannot confirm, so refuse", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
			deps.ResolvePeer = func(string) (PeerSession, bool) {
				return PeerSession{Workspace: "/other/project", ID: "sess-bob"}, true
			}
			globalCreated := false
			var globalStore *collab.Store
			if tc.allows != nil {
				deps.TargetAllowsCrossProject = tc.allows
			}
			deps.GlobalStore = func() *collab.Store {
				globalCreated = true
				return globalStore
			}

			_, err := NewLeaveNote(deps).Execute(context.Background(),
				json.RawMessage(`{"to":"bob","body":"ping"}`))
			if err == nil {
				t.Fatal("expected a refusal error, got success")
			}
			if !strings.Contains(err.Error(), "cross_project") {
				t.Errorf("expected the refusal to name cross_project as the reason; got %q", err.Error())
			}
			if globalCreated {
				t.Error("the cross-project store must never be touched when consent cannot be confirmed")
			}
		})
	}
}

// TestLeaveNote_CrossProjectSendsWhenTargetOptedIn guards the positive case:
// the new consent check must not block a legitimate cross-project send once
// the recipient project has actually opted in.
func TestLeaveNote_CrossProjectSendsWhenTargetOptedIn(t *testing.T) {
	deps, _, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	deps.ResolvePeer = func(string) (PeerSession, bool) {
		return PeerSession{Workspace: "/other/project", ID: "sess-bob"}, true
	}
	deps.TargetAllowsCrossProject = func(ws string) bool { return ws == "/other/project" }
	globalWS := t.TempDir()
	globalStore, err := collab.Open(globalWS)
	if err != nil {
		t.Fatalf("open global store: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	deps.GlobalStore = func() *collab.Store { return globalStore }

	out, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"bob","body":"ping"}`))
	if err != nil {
		t.Fatalf("an opted-in target must not be refused: %v", err)
	}
	if !strings.Contains(out, "Message sent") {
		t.Errorf("expected a success reply; got %q", out)
	}
	pending, err := globalStore.PendingNotes(context.Background(),
		collab.Claimant{Name: "bob", ID: "sess-bob", Workspace: "/other/project"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending in the global store = %d, want 1", len(pending))
	}
}

func TestLeaveNote_MissingBodyRejected(t *testing.T) {
	deps, _, _ := collabTestDeps(t, CollabPolicy{Mailbox: true})
	tool := NewLeaveNote(deps)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"to":"bob"}`)); err == nil {
		t.Fatal("expected an error for a missing body")
	}
}
