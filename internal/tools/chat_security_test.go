package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCollabTools_UnregisteredSessionCannotSendOrClaim(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	put(t, local, "bob", "alice", "for the registered alice", "", "", "")
	deps.SessionID = "" // registration failed, but the fallback display name remains.

	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, "not registered") {
		t.Fatalf("unregistered check_messages did not fail closed: out=%q err=%v", out, err)
	}
	pending, err := local.PendingNotesForSession(
		context.Background(), "alice", "sess-alice", deps.Workspace(), time.Now(),
	)
	if err != nil || len(pending) != 1 {
		t.Fatalf("unregistered reader consumed another session's note: rows=%#v err=%v", pending, err)
	}

	out, err = NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"bob","body":"must not send"}`))
	if err != nil || !strings.Contains(out, "not registered") {
		t.Fatalf("unregistered leave_note did not fail closed: out=%q err=%v", out, err)
	}
}

func TestInboxClaim_FairlyIncludesPermittedGlobalMail(t *testing.T) {
	deps, local, global := chatTestDeps(t, CollabPolicy{Mailbox: true, CrossProject: true}, "alice")
	for _, body := range []string{"local-1", "local-2", "local-3", "local-4"} {
		put(t, local, "bob", "alice", body, "", "", "")
	}
	put(t, global, "carol", "alice", "global-must-not-starve", "", "/other", deps.Workspace())

	rows := (Inbox{
		Self: "alice", SelfID: deps.SessionID, Root: deps.Workspace(),
		Policy: deps.Policy(), Workspace: deps.StoreIfExists, Global: deps.GlobalStoreIfExists,
	}).Claim(context.Background())
	if len(rows) != maxDeliveredPerCall {
		t.Fatalf("claim count = %d, want %d: %#v", len(rows), maxDeliveredPerCall, rows)
	}
	foundGlobal := false
	for _, row := range rows {
		foundGlobal = foundGlobal || row.Body == "global-must-not-starve"
	}
	if !foundGlobal {
		t.Fatalf("busy local store starved global mail: %#v", rows)
	}
}

func TestLeaveNote_FreshActiveNamePersistsStableTarget(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	deps.PeerSessionByName = func(name string) (string, string, bool, bool) {
		return "id-bob", deps.Workspace(), name == "bob", false
	}
	out, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"bob","body":"bound to the original bob"}`))
	if err != nil || !strings.Contains(out, "queued") {
		t.Fatalf("fresh targeted send failed: out=%q err=%v", out, err)
	}

	if rows, err := local.ClaimNotesForSession(
		context.Background(), "bob", "id-attacker", deps.Workspace(), time.Now(), 1,
	); err != nil || len(rows) != 0 {
		t.Fatalf("same-named replacement claimed stable-target mail: rows=%#v err=%v", rows, err)
	}
	rows, err := local.ClaimNotesForSession(
		context.Background(), "robert", "id-bob", deps.Workspace(), time.Now(), 1,
	)
	if err != nil || len(rows) != 1 || rows[0].Body != "bound to the original bob" ||
		rows[0].TargetID != "id-bob" {
		t.Fatalf("renamed intended peer did not receive stable-target mail: rows=%#v err=%v", rows, err)
	}
}

func TestLeaveNote_FreshAmbiguousNameRefuses(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	deps.PeerSessionByName = func(string) (string, string, bool, bool) {
		return "", "", false, true
	}
	_, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"bob","body":"do not guess"}`))
	if err == nil || !strings.Contains(err.Error(), "more than one active session") {
		t.Fatalf("ambiguous live name was not refused: %v", err)
	}
	if rows, err := local.PendingNotes(context.Background(), "bob", deps.Workspace(), time.Now()); err != nil || len(rows) != 0 {
		t.Fatalf("ambiguous send created a row: rows=%#v err=%v", rows, err)
	}
}

func TestLeaveNote_SplitStoreAmbiguityFailsClosed(t *testing.T) {
	deps, local, global := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	conv := put(t, local, "bob", "alice", "local question", "", "", "")
	if rows, err := local.ClaimNotesForSession(
		context.Background(), "alice", deps.SessionID, deps.Workspace(), time.Now(), 1,
	); err != nil || len(rows) != 1 {
		t.Fatalf("establish local participation: rows=%#v err=%v", rows, err)
	}
	put(t, global, "carol", "alice", "global interjection", conv, "/other", deps.Workspace())

	_, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"body":"must not choose","conversation_id":`+jsonStr(conv)+`}`))
	if err == nil || !strings.Contains(err.Error(), "exactly one other stable participant") {
		t.Fatalf("split-store group thread did not fail closed: %v", err)
	}
}
