package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/session"
)

func TestWorkspaceSessions_CollabShowsDeliveryStateAndVolume(t *testing.T) {
	ws := t.TempDir()
	local, err := collab.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	global, err := collab.OpenGlobalAt(filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = local.Close()
		_ = global.Close()
	})
	now := time.Now()

	conv, err := local.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice", Body: "question",
		Addressee: "bob", TTL: time.Hour,
	}, now.Add(-3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob", Body: "answer",
		Addressee: "alice", TTL: time.Hour, ConversationID: conv,
	}, now.Add(-2*time.Second)); err != nil {
		t.Fatal(err)
	}
	deliveredConv, err := local.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice", Body: "done",
		Addressee: "carol", TTL: time.Hour,
	}, now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := local.ClaimNotesForSession(
		context.Background(), "carol", "sess-carol", ws, now, 1,
	); err != nil || len(rows) != 1 {
		t.Fatalf("claim delivered note: rows=%#v err=%v", rows, err)
	}

	crossConv, err := global.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice", Body: "cross",
		Addressee: "dan", TTL: time.Hour, OriginWorkspace: ws, TargetWorkspace: "/other",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := global.ClaimNotesForSession(
		context.Background(), "dan", "sess-dan", "/other", now.Add(time.Second), 1,
	); err != nil || len(rows) != 1 {
		t.Fatalf("claim cross-project note: rows=%#v err=%v", rows, err)
	}

	tool := NewWorkspaceSessions(func() string { return ws }, "sess-alice").
		WithCollab(
			func() (bool, bool) { return false, true },
			func() *collab.Store { return local },
			func() string { return "alice" },
		).
		WithGlobalCollab(func() *collab.Store { return global }, func() bool { return true })
	out := tool.collabBlock(now.Add(2 * time.Second))
	for _, want := range []string{
		"notes for you", "from bob — pending, 6 bytes, conversation " + conv,
		"your recent notes (delivery state)",
		conv + " → bob — pending",
		deliveredConv + " → carol — delivered to carol",
		crossConv + " → dan — delivered to dan",
		"conversation volume", conv + " — 2 notes, 2 pending",
		crossConv + " — 1 note, 0 pending",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("collab observability missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "answer") {
		t.Fatalf("workspace_sessions duplicated a pending note body instead of metadata:\n%s", out)
	}

	globalOnly := NewWorkspaceSessions(func() string { return ws }, "sess-alice").
		WithCollab(
			func() (bool, bool) { return false, true },
			func() *collab.Store { return nil },
			func() string { return "alice" },
		).
		WithGlobalCollab(func() *collab.Store { return global }, func() bool { return true })
	if out := globalOnly.collabBlock(now.Add(2 * time.Second)); !strings.Contains(out, crossConv+" → dan — delivered to dan") ||
		!strings.Contains(out, crossConv+" — 1 note, 0 pending") {
		t.Fatalf("cross-project state or volume disappeared without a local store:\n%s", out)
	}

	globalOnly.collabGlobalVolume = func() bool { return false }
	if out := globalOnly.collabBlock(now.Add(2 * time.Second)); strings.Contains(out, crossConv+" — 1 note, 0 pending") {
		t.Fatalf("cross-project volume bypassed recipient consent:\n%s", out)
	}
}

func TestFormatWorkspaceSessions_NamesHowToAddressActivePeer(t *testing.T) {
	now := time.Now()
	peers := []session.Info{
		{ID: "self", Name: "alice", Folder: "/ws", LastSeenAt: now},
		{ID: "peer", Name: "swift-falcon", Folder: "/ws", LastSeenAt: now},
	}
	out := formatWorkspaceSessions("/ws", "self", peers, nil, nil, now)
	if !strings.Contains(out, "swift-falcon") ||
		!strings.Contains(out, "leave_note.to using its displayed session name") {
		t.Fatalf("active peer addressing guidance missing: %q", out)
	}
}
