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
	got, err := store.DeliverNotes(context.Background(), "whoever", time.Now())
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
	pending, err := store.PendingNotes(context.Background(), "alice", time.Now())
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

func TestLeaveNote_MissingBodyRejected(t *testing.T) {
	deps, _, _ := collabTestDeps(t, CollabPolicy{Mailbox: true})
	tool := NewLeaveNote(deps)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"to":"bob"}`)); err == nil {
		t.Fatal("expected an error for a missing body")
	}
}
