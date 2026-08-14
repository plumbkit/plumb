package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

func TestFileFromInputJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"file_path", `{"file_path":"/ws/foo.go","content":"x"}`, "/ws/foo.go"},
		{"from (rename/copy)", `{"from":"/ws/a.go","to":"/ws/b.go"}`, "/ws/a.go"},
		{"path alias", `{"path":"/ws/p.go"}`, "/ws/p.go"},
		{"transaction first op", `{"operations":[{"file_path":"/ws/t1.go"},{"file_path":"/ws/t2.go"}]}`, "/ws/t1.go"},
		{"move_symbol source_uri", `{"source_uri":"/ws/m.go","name_path":"F","destination_uri":"/ws/n.go"}`, "/ws/m.go"},
		{"git (no path)", `{"subcommand":"commit","message":"x"}`, ""},
		{"empty", ``, ""},
		{"malformed", `{not json`, ""},
		{"empty file_path falls through", `{"file_path":""}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fileFromInputJSON(tt.in); got != tt.want {
				t.Errorf("fileFromInputJSON(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatWorkspaceSessions_Alone(t *testing.T) {
	now := time.Now()
	peers := []session.Info{
		{ID: "self-1", Name: "lone-otter", Folder: "/ws", LastSeenAt: now},
	}
	out := formatWorkspaceSessions("/ws", "self-1", peers, nil, nil, now)

	if !strings.Contains(out, "you:  lone-otter") {
		t.Errorf("expected own session name in output:\n%s", out)
	}
	if !strings.Contains(out, "only active session") {
		t.Errorf("a single peer must report the agent is alone:\n%s", out)
	}
	if !strings.Contains(out, "authoritative") {
		t.Errorf("alone case should note the view is authoritative:\n%s", out)
	}
	if !strings.Contains(out, "recent writes: none recorded") {
		t.Errorf("empty writes should be reported:\n%s", out)
	}
}

func TestFormatWorkspaceSessions_MultiplePeersAndWrites(t *testing.T) {
	now := time.Now()
	peers := []session.Info{
		{ID: "self-1", Name: "me-fox", Folder: "/ws", ClientName: "claude-code", LastSeenAt: now.Add(-10 * time.Second)},
		{ID: "peer-2", Name: "brave-lake", Folder: "/ws", ClientName: "claude-code", LastSeenAt: now.Add(-40 * time.Minute)},
	}
	writes := []stats.RecentCall{
		{Tool: "edit_file", SessionName: "brave-lake", CalledAt: now.Add(-30 * time.Second), Success: true, InputJSON: `{"file_path":"/ws/internal/pool.go"}`},
		{Tool: "git", SessionName: "brave-lake", CalledAt: now.Add(-2 * time.Minute), Success: true, InputJSON: `{"subcommand":"commit"}`},
	}
	out := formatWorkspaceSessions("/ws", "self-1", peers, writes, nil, now)

	if strings.Contains(out, "no change applied") {
		t.Errorf("successful writes must not carry the failure marker:\n%s", out)
	}

	if !strings.Contains(out, "active sessions: 2 (including you)") {
		t.Errorf("expected a count of 2 active sessions:\n%s", out)
	}
	if !strings.Contains(out, "me-fox (you)") {
		t.Errorf("the caller's own session must be marked (you):\n%s", out)
	}
	if !strings.Contains(out, "brave-lake") || !strings.Contains(out, "idle") {
		t.Errorf("the idle peer should be listed and marked idle:\n%s", out)
	}
	// Recent write paths are rendered relative to the workspace.
	if !strings.Contains(out, "internal/pool.go") {
		t.Errorf("expected the relative write path:\n%s", out)
	}
	if strings.Contains(out, "/ws/internal/pool.go") {
		t.Errorf("path should be relative to the workspace, not absolute:\n%s", out)
	}
	// A git write with no path still appears (tool name only).
	if !strings.Contains(out, "git") {
		t.Errorf("a path-less write (git) should still be listed:\n%s", out)
	}
}

// TestFormatWorkspaceSessions_ListsActiveLSPs verifies that every language
// server a session is driving is shown (a multi-language root runs several,
// e.g. gopls + the HTML server), and that older records with only the single
// Adapter field still render.
func TestFormatWorkspaceSessions_ListsActiveLSPs(t *testing.T) {
	now := time.Now()
	peers := []session.Info{
		{
			ID: "self-1", Name: "me-fox", Folder: "/ws", ClientName: "claude-code",
			LastSeenAt: now.Add(-5 * time.Second),
			Adapters:   []string{"gopls", "vscode-html-language-server"},
		},
		{
			ID: "peer-2", Name: "old-rec", Folder: "/ws", ClientName: "claude-code",
			LastSeenAt: now.Add(-5 * time.Second),
			Adapter:    "gopls", // legacy record: only the primary
		},
	}
	out := formatWorkspaceSessions("/ws", "self-1", peers, nil, nil, now)

	if !strings.Contains(out, "LSP gopls, vscode-html-language-server") {
		t.Errorf("expected the full active-LSP set for the multi-language session:\n%s", out)
	}
	if !strings.Contains(out, "old-rec") || !strings.Contains(out, "LSP gopls") {
		t.Errorf("a legacy record with only Adapter should still show its LSP:\n%s", out)
	}
}

// TestWorkspaceSessions_ConcurrentNoDeadlock proves that concurrent Execute
// calls do not deadlock. Each call takes the session-dir flock (LOCK_EX) for a
// brief read then releases — concurrent callers queue behind it, which is
// correct serialisation, not deadlock. The wsSessionsTimeout backstop (500ms)
// inside Execute is a second line of defence, but this test verifies the
// access is inherently safe: all goroutines must finish well inside the
// deadline regardless of which code path fires first.
func TestWorkspaceSessions_ConcurrentNoDeadlock(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const ws = "/tmp/plumb-ws-sessions-concurrency-v3"

	selfID, err := registerID(session.Info{ID: "self-nd", Name: "self-nd", Folder: ws})
	if err != nil {
		t.Fatalf("register self: %v", err)
	}
	if _, err := registerID(session.Info{ID: "peer-nd", Name: "peer-nd", Folder: ws}); err != nil {
		t.Fatalf("register peer: %v", err)
	}

	tool := NewWorkspaceSessions(func() string { return ws }, selfID)

	// 6 goroutines × 4 calls = 24 Execute invocations, each taking the flock
	// once. At ~1ms per flock+read (local tmpfs), 24 serial acquisitions fit
	// inside 1 second by a wide margin; we budget 5s for slow CI.
	const goroutines, callsEach = 6, 4
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range callsEach {
				out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
				if err != nil {
					t.Errorf("Execute returned an error: %v", err)
					return
				}
				if out == "" {
					t.Error("Execute returned empty output")
				}
			}
		})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent workspace_sessions.Execute did not finish in 5s — possible deadlock")
	}
}

func TestFormatWorkspaceSessions_UnknownSelf(t *testing.T) {
	now := time.Now()
	// Self not present in the peer list (e.g. session file not yet flushed).
	peers := []session.Info{
		{ID: "peer-2", Name: "brave-lake", Folder: "/ws", LastSeenAt: now},
	}
	out := formatWorkspaceSessions("/ws", "self-1", peers, nil, nil, now)
	if !strings.Contains(out, "you:  (unknown)") {
		t.Errorf("missing self should render as (unknown):\n%s", out)
	}
}

// TestFormatWorkspaceSessions_CommitAttribution covers the git feed entries:
// a successful commit renders a full attribution line (session, short SHA,
// subject, repo); a failed commit or a non-commit git op keeps the bare line.
func TestFormatWorkspaceSessions_CommitAttribution(t *testing.T) {
	now := time.Now()
	peers := []session.Info{
		{ID: "self-1", Name: "me-fox", Folder: "/ws", LastSeenAt: now},
		{ID: "peer-2", Name: "brave-lake", Folder: "/ws", LastSeenAt: now},
	}
	writes := []stats.RecentCall{
		{
			Tool: "git", SessionName: "brave-lake", CalledAt: now.Add(-time.Minute), Success: true,
			InputJSON:  `{"subcommand":"commit","message":"feat: add guard","repo":"/ws/plumb"}`,
			OutputText: "a1b2c3d feat: add guard",
		},
		// A commit whose output carried a leading cross-session guard warning
		// still attributes — the commit identity is the LAST line.
		{
			Tool: "git", SessionName: "brave-lake", CalledAt: now.Add(-2 * time.Minute), Success: true,
			InputJSON:  `{"subcommand":"commit","message":"fix: typo"}`,
			OutputText: "# plumb-warning: HEAD/branch moved …\ne5f6a7b fix: typo",
		},
		{
			Tool: "git", SessionName: "brave-lake", CalledAt: now.Add(-3 * time.Minute), Success: false,
			InputJSON: `{"subcommand":"commit","message":"blocked"}`,
		},
		{
			Tool: "git", SessionName: "brave-lake", CalledAt: now.Add(-4 * time.Minute), Success: true,
			InputJSON: `{"subcommand":"add","files":["x.go"]}`,
		},
	}
	out := formatWorkspaceSessions("/ws", "self-1", peers, writes, nil, now)

	if !strings.Contains(out, "a1b2c3d feat: add guard") {
		t.Errorf("expected the commit's short SHA and subject:\n%s", out)
	}
	if !strings.Contains(out, "[repo: plumb]") {
		t.Errorf("expected the repo rendered relative to the workspace:\n%s", out)
	}
	if !strings.Contains(out, "e5f6a7b fix: typo") || !strings.Contains(out, "[repo: .]") {
		t.Errorf("a warning-prefixed commit output must still attribute; a missing repo key renders \".\":\n%s", out)
	}
	if strings.Contains(out, "blocked") {
		t.Errorf("a failed commit must not render an attribution line:\n%s", out)
	}
	if got := strings.Count(out, "[repo:"); got != 2 {
		t.Errorf("expected exactly 2 attributed commits, got %d:\n%s", got, out)
	}
	// The failed commit stays in the feed — evidence of peer activity — but is
	// labelled with its subcommand and marked as not applied.
	failed := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "git commit") && !strings.Contains(line, "[repo:") {
			failed = line
		}
	}
	if failed == "" || !strings.Contains(failed, "failed — no change applied") {
		t.Errorf("the failed commit must render marked as not applied, got %q in:\n%s", failed, out)
	}
	if !strings.Contains(out, "git add") {
		t.Errorf("a non-commit git write should be labelled with its subcommand:\n%s", out)
	}
}

// registerID registers info and returns just the session ID — Register itself
// returns the completed record so its caller can read back the assigned name.
func registerID(info session.Info) (string, error) {
	reg, err := session.Register(info)
	return reg.ID, err
}

// TestWorkspaceSessions_NotesListingIsBoundToThisSession pins the listing half
// of the addressee binding at its wiring. The store's predicate is only as good
// as the identity handed to it: passing the name without this session's id would
// list a predecessor's unread mail — sender and body — to whoever inherited the
// name, and would also hide a session's own bound mail from it.
func TestWorkspaceSessions_NotesListingIsBoundToThisSession(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	store, err := collab.Open(ws)
	if err != nil {
		t.Fatalf("open collab store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "bob", AuthorID: "id-bob", Body: "bound-body-marker",
		Addressee: "alice", AddresseeID: "sess-alice-1", TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatalf("PutNote: %v", err)
	}

	listFor := func(selfID string) string {
		t.Helper()
		tool := NewWorkspaceSessions(func() string { return ws }, selfID).
			WithCollab(
				func() (bool, bool) { return false, true },
				func() *collab.Store { return store },
				func() string { return "alice" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return out
	}

	if out := listFor("sess-alice-1"); !strings.Contains(out, "bound-body-marker") {
		t.Errorf("the session the message is bound to must see it listed; got %q", out)
	}
	if out := listFor("sess-alice-2"); strings.Contains(out, "bound-body-marker") {
		t.Errorf("a session reusing the name was shown its predecessor's mail; got %q", out)
	}
}

// TestWorkspaceSessions_ListsInheritedMail keeps the listing in step with the
// delivery paths after a daemon restart. A reconnected session that inherited
// its predecessor's identity receives its bound mail; if the listing did not
// apply the same identities it would report an empty mailbox while the messages
// were being handed over on the next tool call.
func TestWorkspaceSessions_ListsInheritedMail(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	store, err := collab.Open(ws)
	if err != nil {
		t.Fatalf("open collab store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "bob", AuthorID: "id-bob", Body: "inherited-body-marker",
		Addressee: "alice", AddresseeID: "sess-before-restart", TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatalf("PutNote: %v", err)
	}

	listFor := func(inherited []string) string {
		t.Helper()
		out, err := NewWorkspaceSessions(func() string { return ws }, "sess-after-restart").
			WithInheritedSessions(func() []string { return inherited }).
			WithCollab(
				func() (bool, bool) { return false, true },
				func() *collab.Store { return store },
				func() string { return "alice" }).
			Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return out
	}

	if out := listFor([]string{"sess-before-restart"}); !strings.Contains(out, "inherited-body-marker") {
		t.Errorf("a session that inherited its predecessor must see its mail listed; got %q", out)
	}
	if out := listFor(nil); strings.Contains(out, "inherited-body-marker") {
		t.Errorf("a session with no inheritance was shown the bound message; got %q", out)
	}
}

// TestWorkspaceSessions_UnregisteredSessionIsShownNoMail closes the second
// disclosure lane. addressee_id protects BOUND rows here, but an unbound row —
// a pre-v3 note, or one to a peer that had not connected — is matched by name
// alone, and this block prints the sender and the body. An unregistered session
// carries a display name drawn without a uniqueness check, so it can shadow a
// live peer; listing consumes nothing, which is precisely why it was overlooked.
func TestWorkspaceSessions_UnregisteredSessionIsShownNoMail(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	store, err := collab.Open(ws)
	if err != nil {
		t.Fatalf("open collab store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// UNBOUND: addressed by name only, which is what addressee_id cannot protect.
	if _, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "bob", AuthorID: "id-bob", Body: "unbound-body-marker",
		Addressee: "alice", TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatalf("PutNote: %v", err)
	}

	listFor := func(selfID, selfName string) string {
		t.Helper()
		out, err := NewWorkspaceSessions(func() string { return ws }, selfID).
			WithCollab(
				func() (bool, bool) { return false, true },
				func() *collab.Store { return store },
				func() string { return selfName }).
			Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return out
	}

	// A registered session holding the name legitimately sees it.
	if out := listFor("sess-alice", "alice"); !strings.Contains(out, "unbound-body-marker") {
		t.Fatalf("a registered session must see its own pending mail; got %q", out)
	}
	// An unregistered shadow must not — not the body, and not the sender.
	out := listFor("", "alice")
	if strings.Contains(out, "unbound-body-marker") {
		t.Errorf("an unregistered session was shown a live peer's message body; got %q", out)
	}
	if strings.Contains(out, "notes for you") {
		t.Errorf("an unregistered session was shown a notes block at all; got %q", out)
	}
}
