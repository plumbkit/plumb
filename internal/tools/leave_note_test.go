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

func TestLeaveNote_MissingBodyRejected(t *testing.T) {
	deps, _, _ := collabTestDeps(t, CollabPolicy{Mailbox: true})
	tool := NewLeaveNote(deps)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"to":"bob"}`)); err == nil {
		t.Fatal("expected an error for a missing body")
	}
}
