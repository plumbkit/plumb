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

// workspace_sessions_collab_test.go covers the two observational sections and,
// more importantly, the disclosure rule attached to one of them: cross-project
// conversation metadata is shown only to the RECIPIENT project, and only when
// that project has opted in.

// observabilityTool builds the listing tool with both observational sections
// wired. crossProject is THIS workspace's consent.
func observabilityTool(
	t *testing.T, ws, selfID, selfName string, local, global *collab.Store, crossProject bool,
) *WorkspaceSessions {
	t.Helper()
	return NewWorkspaceSessions(func() string { return ws }, selfID).
		WithCollab(
			func() (bool, bool) { return false, true },
			func() *collab.Store { return local },
			func() string { return selfName },
		).
		WithCollabObservability(
			func() *collab.Store { return global },
			func() bool { return crossProject },
		)
}

func openStore(t *testing.T, dir string) *collab.Store {
	t.Helper()
	s, err := collab.Open(dir)
	if err != nil {
		t.Fatalf("open collab store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openGlobalStore opens a REAL daemon-level store. It must be OpenGlobalAt and
// not Open: the target-workspace addressing rules — and so the whole consent
// gate below — apply only to a store whose IsGlobal reports true, and a
// workspace store standing in for one would make every cross-project assertion
// here pass for the wrong reason.
func openGlobalStore(t *testing.T) *collab.Store {
	t.Helper()
	s, err := collab.OpenGlobalAt(filepath.Join(t.TempDir(), "collab-xproject.db"))
	if err != nil {
		t.Fatalf("open global collab store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func listing(t *testing.T, tool *WorkspaceSessions) string {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

// TestWorkspaceSessions_SentNotesShowDeliveryStateWithoutBodies is the sender's
// half. check_messages already reports unread sent mail, but only when called
// and only the failures — so a session that never calls it learns nothing, and
// one that does cannot tell "nothing unread" from "sent nothing".
func TestWorkspaceSessions_SentNotesShowDeliveryState(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	store := openStore(t, ws)
	ctx, now := context.Background(), time.Now()

	if _, err := store.PutNote(ctx, collab.NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "SECRET-BODY-MARKER", Addressee: "bob", TTL: time.Hour,
	}, now); err != nil {
		t.Fatal(err)
	}

	out := listing(t, observabilityTool(t, ws, "sess-alice", "alice", store, nil, false))

	if !strings.Contains(out, "your recent notes") {
		t.Fatalf("sent section missing:\n%s", out)
	}
	if !strings.Contains(out, "pending") {
		t.Errorf("an unread note should read as pending:\n%s", out)
	}
	if !strings.Contains(out, "to bob") {
		t.Errorf("the addressee should be named:\n%s", out)
	}
	// The rule that makes this section safe to render at all.
	if strings.Contains(out, "SECRET-BODY-MARKER") {
		t.Errorf("the sent section printed a message body:\n%s", out)
	}
}

// TestWorkspaceSessions_SentNotesReportDeliveredState: "delivered" is the fact
// the section exists to show, so a read note must change what it says.
func TestWorkspaceSessions_SentNotesReportDeliveredState(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	store := openStore(t, ws)
	ctx, now := context.Background(), time.Now()

	if _, err := store.PutNote(ctx, collab.NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "hello", Addressee: "bob", TTL: time.Hour,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNotes(ctx, collab.Claimant{Name: "bob", ID: "sess-bob"}, now, 0); err != nil {
		t.Fatal(err)
	}

	out := listing(t, observabilityTool(t, ws, "sess-alice", "alice", store, nil, false))
	if !strings.Contains(out, "delivered to bob") {
		t.Errorf("a claimed note should report delivery:\n%s", out)
	}
}

// TestWorkspaceSessions_SentNotesAreOnlyTheCallersOwn: the section is rendered
// without any consent gate precisely because it holds only what the caller
// wrote. If another session's rows appeared, that reasoning would be void.
func TestWorkspaceSessions_SentNotesAreOnlyTheCallersOwn(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	store := openStore(t, ws)
	ctx, now := context.Background(), time.Now()

	if _, err := store.PutNote(ctx, collab.NoteInput{
		AuthorSession: "carol", AuthorID: "sess-carol",
		Body: "carols note", Addressee: "dave", TTL: time.Hour,
	}, now); err != nil {
		t.Fatal(err)
	}

	out := listing(t, observabilityTool(t, ws, "sess-alice", "alice", store, nil, false))
	if strings.Contains(out, "to dave") {
		t.Errorf("the sent section showed a note this session did not write:\n%s", out)
	}
}

// TestWorkspaceSessions_ConversationVolumeIsObservationalOnly: the section
// replaces an enforcement mechanism, so its wording has to make clear it does
// not enforce. A reader who mistakes a count for a cap will go looking for the
// knob that raises it.
func TestWorkspaceSessions_ConversationVolumeIsObservationalOnly(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	store := openStore(t, ws)
	ctx, now := context.Background(), time.Now()

	conv, err := store.PutNote(ctx, collab.NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "one", Addressee: "bob", TTL: time.Hour,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutNote(ctx, collab.NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob",
		Body: "two", Addressee: "alice", ConversationID: conv, TTL: time.Hour,
	}, now); err != nil {
		t.Fatal(err)
	}

	out := listing(t, observabilityTool(t, ws, "sess-alice", "alice", store, nil, false))
	if !strings.Contains(out, "conversation volume") {
		t.Fatalf("volume section missing:\n%s", out)
	}
	if !strings.Contains(out, "observational only") {
		t.Errorf("the volume section must not read as a control:\n%s", out)
	}
	if !strings.Contains(out, "2 note(s)") {
		t.Errorf("expected the thread counted at 2:\n%s", out)
	}
}

// TestWorkspaceSessions_CrossProjectVolumeNeedsRecipientConsent is the rule this
// file exists for.
//
// The consent that matters is the RECIPIENT's, not the sender's. A sender
// choosing to write across projects cannot consent on the recipient's behalf to
// that traffic being visible inside the recipient's project — the recipient is
// the one whose screen it appears on. The same rule already governs whether the
// message is delivered at all; this stops the metadata becoming a way around it.
func TestWorkspaceSessions_CrossProjectVolumeNeedsRecipientConsent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	local := openStore(t, ws)
	global := openGlobalStore(t)
	ctx, now := context.Background(), time.Now()

	// A note from another project, aimed at this workspace, in the daemon store.
	if _, err := global.PutNote(ctx, collab.NoteInput{
		AuthorSession: "stranger", AuthorID: "sess-stranger",
		Body: "from elsewhere", Addressee: "alice",
		OriginWorkspace: "/other/project", TargetWorkspace: ws, TTL: time.Hour,
	}, now); err != nil {
		t.Fatal(err)
	}

	withoutConsent := listing(t, observabilityTool(t, ws, "sess-alice", "alice", local, global, false))
	if strings.Contains(withoutConsent, "conversation volume") {
		t.Errorf("cross-project volume was disclosed to a project that has not opted in:\n%s", withoutConsent)
	}

	withConsent := listing(t, observabilityTool(t, ws, "sess-alice", "alice", local, global, true))
	if !strings.Contains(withConsent, "conversation volume") {
		t.Errorf("a project that opted in should see the traffic aimed at it:\n%s", withConsent)
	}
}

// TestWorkspaceSessions_CrossProjectVolumeIsScopedToThisWorkspace: consent buys
// visibility of traffic aimed AT this project, not of every project's traffic
// on the machine. The daemon store holds all of it.
func TestWorkspaceSessions_CrossProjectVolumeIsScopedToThisWorkspace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	local := openStore(t, ws)
	global := openGlobalStore(t)
	ctx, now := context.Background(), time.Now()

	// Traffic between two OTHER projects, which this one must never be shown.
	if _, err := global.PutNote(ctx, collab.NoteInput{
		AuthorSession: "stranger", AuthorID: "sess-stranger",
		Body: "not for you", Addressee: "someone",
		OriginWorkspace: "/other/a", TargetWorkspace: "/other/b", TTL: time.Hour,
	}, now); err != nil {
		t.Fatal(err)
	}

	out := listing(t, observabilityTool(t, ws, "sess-alice", "alice", local, global, true))
	if strings.Contains(out, "conversation volume") {
		t.Errorf("a third party's cross-project traffic was disclosed:\n%s", out)
	}
}

// TestWorkspaceSessions_UnwiredConsentDisclosesNothing is the fail-closed
// default, and it is not covered by the test below it: passing a global store
// with a NIL consent accessor is the shape an embedder produces by forgetting
// one argument, and the wrong default there discloses another project's traffic
// silently. Caught by mutation testing — flipping crossProjectOn to treat nil as
// consent survived every other test in this file.
func TestWorkspaceSessions_UnwiredConsentDisclosesNothing(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	local := openStore(t, ws)
	global := openGlobalStore(t)
	ctx, now := context.Background(), time.Now()

	if _, err := global.PutNote(ctx, collab.NoteInput{
		AuthorSession: "stranger", AuthorID: "sess-stranger",
		Body: "from elsewhere", Addressee: "alice",
		OriginWorkspace: "/other/project", TargetWorkspace: ws, TTL: time.Hour,
	}, now); err != nil {
		t.Fatal(err)
	}

	tool := NewWorkspaceSessions(func() string { return ws }, "sess-alice").
		WithCollab(
			func() (bool, bool) { return false, true },
			func() *collab.Store { return local },
			func() string { return "alice" },
		).
		// Store wired, consent NOT wired.
		WithCollabObservability(func() *collab.Store { return global }, nil)

	if out := listing(t, tool); strings.Contains(out, "conversation volume") {
		t.Errorf("an unwired consent accessor disclosed cross-project traffic:\n%s", out)
	}
}

// TestWorkspaceSessions_ObservabilityUnwiredChangesNothing: both accessors are
// nil-safe and default to off, so an embedder that does not wire them gets the
// listing exactly as it was.
func TestWorkspaceSessions_ObservabilityUnwiredChangesNothing(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	store := openStore(t, ws)
	ctx, now := context.Background(), time.Now()

	if _, err := store.PutNote(ctx, collab.NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice",
		Body: "hello", Addressee: "bob", TTL: time.Hour,
	}, now); err != nil {
		t.Fatal(err)
	}

	tool := NewWorkspaceSessions(func() string { return ws }, "sess-alice").
		WithCollab(
			func() (bool, bool) { return false, true },
			func() *collab.Store { return store },
			func() string { return "alice" },
		)
	out := listing(t, tool)

	// The sections still render from the local store — that needs no wiring —
	// but nothing reaches for a global store that was never provided.
	if strings.Contains(out, "from elsewhere") {
		t.Errorf("unwired observability reached a store it was not given:\n%s", out)
	}
}
