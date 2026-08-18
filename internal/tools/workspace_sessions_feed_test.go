package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

// workspace_sessions_feed_test.go — accuracy of the recent-writes feed. The
// feed's contract is that it shows what peers WROTE: a reader must be able to
// tell a write that landed from a call that was refused, and a read-only
// operation (a read-tier git subcommand, a dry-run preview) must not appear as
// a write at all. Assertions are on the rendered feed, not internal booleans.

// renderFeed runs raw stats rows through the feed filter and the renderer,
// exactly as runSync does, and returns the rendered output.
func renderFeed(writes []stats.RecentCall, limit int) string {
	now := time.Now()
	peers := []session.Info{{ID: "self-1", Name: "me-fox", Folder: "/ws", LastSeenAt: now}}
	return formatWorkspaceSessions("/ws", "self-1", peers, feedRecentWrites(writes, limit), nil, now)
}

// feedLine returns the first rendered line containing needle, or "" when none does.
func feedLine(out, needle string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// TestRecentWritesFeed_RefusedWriteIsMarked drives a refused write through the
// real recording path (stats DB → RecentWritesByWorkspace → feed filter →
// renderer) and asserts the feed does not present it as a landed write: the
// entry stays (a refused write is still evidence a peer is working in that
// file) but carries an explicit no-change marker, while a landed write does not.
func TestRecentWritesFeed_RefusedWriteIsMarked(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := stats.Open()
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	calls := []stats.Call{
		{
			SessionID: "s1", SessionName: "brave-lake", Workspace: "/ws", Tool: "edit_file",
			CalledAt: now.Add(-time.Minute), Success: false,
			ErrorMsg:  "file has uncommitted changes (pass dirty_ok to override)",
			InputJSON: `{"file_path":"/ws/internal/dirty.go"}`,
		},
		{
			SessionID: "s1", SessionName: "brave-lake", Workspace: "/ws", Tool: "edit_file",
			CalledAt: now.Add(-2 * time.Minute), Success: true,
			InputJSON: `{"file_path":"/ws/internal/landed.go"}`,
		},
	}
	for _, c := range calls {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	writes, err := db.RecentWritesByWorkspace("/ws", writeToolNames, 10)
	if err != nil {
		t.Fatalf("RecentWritesByWorkspace: %v", err)
	}
	if len(writes) != 2 {
		t.Fatalf("RecentWritesByWorkspace returned %d rows, want 2", len(writes))
	}
	out := renderFeed(writes, 10)

	refused := feedLine(out, "internal/dirty.go")
	if refused == "" {
		t.Fatalf("the refused write must still appear in the feed (it is evidence of peer activity):\n%s", out)
	}
	if !strings.Contains(refused, "failed — no change applied") {
		t.Errorf("a refused write must be marked as not applied, got line %q in:\n%s", refused, out)
	}
	landed := feedLine(out, "internal/landed.go")
	if landed == "" || strings.Contains(landed, "failed") {
		t.Errorf("a landed write must render without a failure marker, got line %q in:\n%s", landed, out)
	}
}

// TestRecentWritesFeed_ReadOnlyGitExcluded: read-tier git subcommands (status,
// log, diff, branch --list) must not appear in a feed of writes; write-tier git
// (add, branch creation) stays, labelled with its subcommand. Classification
// must consult the args, not just the subcommand — `branch --list` reads while
// `branch <name>` writes.
func TestRecentWritesFeed_ReadOnlyGitExcluded(t *testing.T) {
	now := time.Now()
	mk := func(age time.Duration, input string) stats.RecentCall {
		return stats.RecentCall{
			Tool: "git", SessionName: "brave-lake", CalledAt: now.Add(-age),
			Success: true, InputJSON: input,
		}
	}
	writes := []stats.RecentCall{
		mk(1*time.Minute, `{"subcommand":"status"}`),
		mk(2*time.Minute, `{"subcommand":"log","args":["--oneline","-10"]}`),
		mk(3*time.Minute, `{"subcommand":"diff","args":["--cached"]}`),
		mk(4*time.Minute, `{"subcommand":"branch","args":["--list"]}`),
		mk(5*time.Minute, `{"subcommand":"add","files":["x.go"]}`),
		mk(6*time.Minute, `{"subcommand":"branch","args":["feature/x"]}`),
		mk(7*time.Minute, `{not json`),
	}
	out := renderFeed(writes, 10)

	for _, absent := range []string{"git status", "git log", "git diff"} {
		if strings.Contains(out, absent) {
			t.Errorf("read-only %q must not appear in the recent-writes feed:\n%s", absent, out)
		}
	}
	if !strings.Contains(out, "git add") {
		t.Errorf("a write-tier git call must stay in the feed, labelled with its subcommand:\n%s", out)
	}
	if got := strings.Count(out, "git branch"); got != 1 {
		t.Errorf("only the branch CREATION belongs in the feed (got %d 'git branch' lines) — classification must consult args:\n%s", got, out)
	}
}

// TestRecentWritesFeed_DryRunPreviewExcluded: for tools whose dry_run defaults
// to true, a recorded call without an explicit dry_run=false was a preview that
// wrote nothing and must not appear as a write. An applied call (dry_run=false)
// stays; tools with no dry-run concept are unaffected.
func TestRecentWritesFeed_DryRunPreviewExcluded(t *testing.T) {
	now := time.Now()
	writes := []stats.RecentCall{
		{
			Tool: "rename_symbol", SessionName: "p", CalledAt: now.Add(-time.Minute), Success: true,
			InputJSON: `{"uri":"/ws/a.go","new_name":"B"}`,
		}, // default dry-run → preview
		{
			Tool: "find_replace", SessionName: "p", CalledAt: now.Add(-2 * time.Minute), Success: true,
			InputJSON: `{"path":"/ws","pattern":"x","replacement":"y","dry_run":true}`,
		}, // explicit preview
		{
			Tool: "find_replace", SessionName: "p", CalledAt: now.Add(-3 * time.Minute), Success: true,
			InputJSON: `{"path":"/ws","pattern":"x","replacement":"y","dry_run":false}`,
		}, // applied
		{
			Tool: "edit_file", SessionName: "p", CalledAt: now.Add(-4 * time.Minute), Success: true,
			InputJSON: `{"file_path":"/ws/c.go"}`,
		}, // no dry-run concept
	}
	out := renderFeed(writes, 10)

	if strings.Contains(out, "rename_symbol") {
		t.Errorf("a default-dry-run preview must not appear as a write:\n%s", out)
	}
	if got := strings.Count(out, "find_replace"); got != 1 {
		t.Errorf("only the APPLIED find_replace belongs in the feed, got %d entries:\n%s", got, out)
	}
	if !strings.Contains(out, "c.go") {
		t.Errorf("a tool with no dry-run concept must stay in the feed:\n%s", out)
	}
}

// TestRecentWritesFeed_TruncatesToLimitAfterFiltering: the requested limit
// applies to the FILTERED feed — dropped read-only rows must not consume feed
// slots, and the survivors are the newest writes.
func TestRecentWritesFeed_TruncatesToLimitAfterFiltering(t *testing.T) {
	now := time.Now()
	writes := make([]stats.RecentCall, 0, 9)
	for i := range 6 {
		writes = append(writes, stats.RecentCall{
			Tool: "git", SessionName: "p", CalledAt: now.Add(-time.Duration(i) * time.Second),
			Success: true, InputJSON: `{"subcommand":"status"}`,
		})
	}
	for _, f := range []string{"/ws/n1.go", "/ws/n2.go", "/ws/n3.go"} {
		writes = append(writes, stats.RecentCall{
			Tool: "edit_file", SessionName: "p", CalledAt: now.Add(-time.Minute),
			Success: true, InputJSON: `{"file_path":"` + f + `"}`,
		})
	}
	out := renderFeed(writes, 2)

	if got := strings.Count(out, "edit_file"); got != 2 {
		t.Errorf("limit must apply after filtering (want 2 edit_file entries, got %d):\n%s", got, out)
	}
	if strings.Contains(out, "git") && strings.Contains(out, "status") {
		t.Errorf("read-only git rows must not appear nor consume feed slots:\n%s", out)
	}
	if !strings.Contains(out, "n1.go") || !strings.Contains(out, "n2.go") || strings.Contains(out, "n3.go") {
		t.Errorf("the two newest writes must survive truncation:\n%s", out)
	}
}

// TestLandedWrites_OnlyAppliedWrites covers the helper behind the peer-hint
// and session_start peer-digest paths, whose wording claims observed edits as
// facts: a refused call, a read-tier git call, and a dry-run preview must all
// be excluded — only writes that actually landed remain.
func TestLandedWrites_OnlyAppliedWrites(t *testing.T) {
	writes := []stats.RecentCall{
		{Tool: "edit_file", Success: false, InputJSON: `{"file_path":"/ws/refused.go"}`},
		{Tool: "edit_file", Success: true, InputJSON: `{"file_path":"/ws/landed.go"}`},
		{Tool: "git", Success: true, InputJSON: `{"subcommand":"status"}`},
		{Tool: "find_replace", Success: true, InputJSON: `{"path":"/ws","pattern":"x"}`},
		{Tool: "git", Success: true, InputJSON: `{"subcommand":"commit","message":"m"}`},
	}
	got := LandedWrites(writes)
	if len(got) != 2 {
		t.Fatalf("LandedWrites returned %d rows, want 2 (the landed edit and the commit): %+v", len(got), got)
	}
	if got[0].InputJSON != `{"file_path":"/ws/landed.go"}` || got[1].Tool != "git" {
		t.Errorf("LandedWrites kept the wrong rows: %+v", got)
	}
}

// TestWorkspaceSessions_ExecuteFiltersAndMarks drives the WIRING end-to-end:
// Execute → stats DB → feed filter → renderer. A read-tier git call recorded
// on the workspace must not surface in the tool's real output, and a refused
// write must surface marked. Guards runSync actually routing rows through
// feedRecentWrites (the pure-function tests above cannot see that wiring).
func TestWorkspaceSessions_ExecuteFiltersAndMarks(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()

	selfID, err := registerID(session.Info{ID: "self-feed", Name: "self-feed", Folder: ws})
	if err != nil {
		t.Fatalf("register self: %v", err)
	}
	db, err := stats.Open()
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	defer db.Close()
	now := time.Now()
	calls := []stats.Call{
		{
			SessionID: "peer", SessionName: "brave-lake", Workspace: ws, Tool: "git",
			CalledAt: now.Add(-time.Minute), Success: true, InputJSON: `{"subcommand":"status"}`,
		},
		{
			SessionID: "peer", SessionName: "brave-lake", Workspace: ws, Tool: "edit_file",
			CalledAt: now.Add(-2 * time.Minute), Success: false,
			ErrorMsg: "file has uncommitted changes", InputJSON: `{"file_path":"` + ws + `/dirty.go"}`,
		},
		{
			SessionID: "peer", SessionName: "brave-lake", Workspace: ws, Tool: "edit_file",
			CalledAt: now.Add(-3 * time.Minute), Success: true, InputJSON: `{"file_path":"` + ws + `/landed.go"}`,
		},
	}
	for _, c := range calls {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	tool := NewWorkspaceSessions(func() string { return ws }, func() string { return selfID })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "git status") {
		t.Errorf("a read-tier git call surfaced in the live recent-writes feed:\n%s", out)
	}
	if refused := feedLine(out, "dirty.go"); refused == "" || !strings.Contains(refused, "failed — no change applied") {
		t.Errorf("the refused write must surface marked, got %q in:\n%s", refused, out)
	}
	if landed := feedLine(out, "landed.go"); landed == "" || strings.Contains(landed, "failed") {
		t.Errorf("the landed write must surface unmarked, got %q in:\n%s", landed, out)
	}
}

// TestWriteToolNames_CoverMutatingTools pins move_symbol into the feed's
// tool set (it relocates declarations across two files) — its absence meant a
// real landed write never appeared in the feed at all.
func TestWriteToolNames_CoverMutatingTools(t *testing.T) {
	if !slices.Contains(WriteToolNames(), "move_symbol") {
		t.Errorf("move_symbol mutates two files and must be part of the recent-writes tool set: %v", WriteToolNames())
	}
}
