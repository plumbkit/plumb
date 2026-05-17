package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/stats"
)

// mkCall builds a RecentCall with the given session id and a CalledAt
// derived from msOffset. Helper kept tiny so test intent stays obvious.
func mkCall(sess string, msOffset int64) stats.RecentCall {
	return stats.RecentCall{
		SessionID: sess,
		CalledAt:  time.UnixMilli(1_000_000_000_000 + msOffset),
	}
}

// Selecting a call and then having newer calls prepend should NOT shift the
// user's selection to a different call — locateCall must follow the original
// row by (session_id, called_at), not by raw index.
func TestLocateCall_PreservesSelectionAcrossPrepend(t *testing.T) {
	before := []stats.RecentCall{
		mkCall("s1", 200),
		mkCall("s1", 150),
		mkCall("s1", 100),
	}
	key := selectedCallKey(before, 1) // user is on the 150ms row

	after := []stats.RecentCall{
		mkCall("s1", 300), // new call prepended
		mkCall("s1", 250), // new call prepended
		mkCall("s1", 200),
		mkCall("s1", 150), // selected row — now at index 3
		mkCall("s1", 100),
	}
	got := locateCall(after, key, 1)
	if got != 3 {
		t.Errorf("locateCall = %d, want 3 (the row at 150ms must still be selected)", got)
	}
}

// When the selected call rolls off the 50-row Recent() limit, locateCall
// falls back to the clamped previous index instead of jumping to 0 —
// otherwise scroll-to-bottom users would snap back up on every refresh.
func TestLocateCall_FallsBackWhenRolledOff(t *testing.T) {
	before := []stats.RecentCall{mkCall("s1", 100), mkCall("s1", 50)}
	key := selectedCallKey(before, 1)
	after := []stats.RecentCall{mkCall("s1", 300)} // 100ms and 50ms gone
	got := locateCall(after, key, 1)
	if got != 0 {
		t.Errorf("locateCall fallback = %d, want 0 (clamped to last index)", got)
	}
}

func TestLocateCall_EmptyList(t *testing.T) {
	got := locateCall(nil, callKey{}, 5)
	if got != 0 {
		t.Errorf("locateCall(nil) = %d, want 0", got)
	}
}

// Two distinct sessions can share the same called_at millisecond — sessionID
// is what disambiguates. locateCall must match on both, not just the time.
func TestLocateCall_DistinguishesSessions(t *testing.T) {
	calls := []stats.RecentCall{
		mkCall("s1", 100),
		mkCall("s2", 100),
	}
	key := callKey{sessionID: "s2", calledAtMs: time.UnixMilli(1_000_000_000_100).UnixMilli()}
	got := locateCall(calls, key, 0)
	if got != 1 {
		t.Errorf("locateCall = %d, want 1 (must match by sessionID, not just time)", got)
	}
}

func TestLocateTool_PreservesSelection(t *testing.T) {
	before := []stats.ToolStat{{Tool: "edit_file"}, {Tool: "read_file"}}
	got := locateTool(before, "read_file", 0)
	if got != 1 {
		t.Errorf("locateTool = %d, want 1", got)
	}
}

func TestLocateTool_RemovedToolClampsToLast(t *testing.T) {
	stats := []stats.ToolStat{{Tool: "edit_file"}}
	got := locateTool(stats, "gone_tool", 3)
	if got != 0 {
		t.Errorf("locateTool with removed tool = %d, want 0 (clamped)", got)
	}
}

func TestLeftLines_RenderSessionsAsTwoLineRows(t *testing.T) {
	RebuildStyles()
	m := Model{
		leftWidth: 42,
		sessions: []session.Info{
			{Name: "CRAZY-PLUMB", Language: "go", Folder: "/Users/gilberto/Projects/plumb"},
			{Name: "SUPER-FRIEND", Language: "go", Folder: "/Users/gilberto/Projects/plumb"},
		},
	}

	lines := m.leftLines()
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = ansiStripForTest(line)
	}
	joined := strings.Join(plain, "\n")
	for _, want := range []string{
		" ● CRAZY-PLUMB  go ",
		"    ╰─ ~/Projects/plumb",
		" ○ SUPER-FRIEND  go ",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("leftLines missing %q in:\n%s", want, joined)
		}
	}
}

func TestMouseDragDividerResizesLeftPanel(t *testing.T) {
	m := Model{leftWidth: 42, width: 100, height: 30}

	updated, _ := m.Update(tea.MouseClickMsg(tea.Mouse{X: 43, Y: 10, Button: tea.MouseLeft}))
	m = updated.(Model)
	if !m.draggingDivider {
		t.Fatal("expected divider drag to start")
	}
	updated, _ = m.Update(tea.MouseMotionMsg(tea.Mouse{X: 50, Y: 10, Button: tea.MouseLeft}))
	m = updated.(Model)
	if m.leftWidth != 49 {
		t.Fatalf("leftWidth = %d, want 49", m.leftWidth)
	}
	updated, _ = m.Update(tea.MouseReleaseMsg(tea.Mouse{X: 50, Y: 10, Button: tea.MouseLeft}))
	m = updated.(Model)
	if m.draggingDivider {
		t.Fatal("expected divider drag to stop")
	}
}

func TestRenderTopMenuUsesVerticalRows(t *testing.T) {
	RebuildStyles()
	m := Model{}

	lines := m.renderTopMenu(20, false)
	if len(lines) != 3 {
		t.Fatalf("renderTopMenu returned %d lines, want 3", len(lines))
	}
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = ansiStripForTest(line)
		if !strings.HasPrefix(plain[i], " ") {
			t.Fatalf("line %d = %q, want leading space", i, plain[i])
		}
		if strings.ContainsAny(plain[i], "▄▀") {
			t.Fatalf("line %d contains old tab box glyphs: %q", i, plain[i])
		}
	}
	for i, want := range []string{" ○ Home", " ● Sessions", " ○ Logs"} {
		if !strings.HasPrefix(plain[i], want) {
			t.Fatalf("line %d = %q, want prefix %q", i, plain[i], want)
		}
	}
}

func ansiStripForTest(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc && r == 'm':
			inEsc = false
		case inEsc:
		case r == '\x1b':
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
