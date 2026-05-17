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

func TestRenderTopMenuUsesRailAndActivityBox(t *testing.T) {
	RebuildStyles()
	m := Model{
		currentSection: 1,
		activity: stats.ActivitySummary{
			Calls:   5200,
			Buckets: []int64{0, 1, 2, 3, 2, 1, 0, 0, 3, 4, 5, 4, 3, 2, 1, 0},
		},
		tokenSavings: 913000,
	}

	lines := m.renderTopMenu(60, false)
	if len(lines) != 3 {
		t.Fatalf("renderTopMenu returned %d lines, want 3", len(lines))
	}
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = ansiStripForTest(line)
		if strings.Contains(plain[i], "▀") {
			t.Fatalf("line %d contains old tab box glyphs: %q", i, plain[i])
		}
	}
	for i, want := range []string{
		"╭────────────────╮ ╭─ Activity (1m) ────────────╮",
		"│ ❯ Sessions   ▾ │ │ ",
		"╰────────────────╯ ╰────────────────────────────╯",
	} {
		if !strings.HasPrefix(plain[i], want) {
			t.Fatalf("line %d = %q, want prefix %q", i, plain[i], want)
		}
	}
	if !strings.Contains(plain[1], "5.2k calls") {
		t.Fatalf("activity row = %q, want call count", plain[1])
	}

	lines = m.renderTopMenu(96, false)
	plain = make([]string, len(lines))
	for i, line := range lines {
		plain[i] = ansiStripForTest(line)
	}
	if !strings.Contains(plain[0], "Tokens Saved") {
		t.Fatalf("top menu = %#v, want tokens saved box title", plain)
	}
	if !strings.Contains(plain[1], "913k") {
		t.Fatalf("token savings row = %q, want savings value", plain[1])
	}
	if !strings.Contains(plain[0], "╮ ╭─ Tokens Saved") {
		t.Fatalf("top menu = %q, want one-space widget gap", plain[0])
	}
}

func TestSectionSelectorKeyFlow(t *testing.T) {
	m := NewModel()

	updated, _ := m.Update(keyPress("/"))
	m = updated.(Model)
	if !m.sectionMenuOpen {
		t.Fatal("section menu did not open")
	}
	if m.sectionMenuCursor != 1 {
		t.Fatalf("sectionMenuCursor = %d, want Sessions index", m.sectionMenuCursor)
	}

	updated, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = updated.(Model)
	if m.sectionMenuCursor != 2 {
		t.Fatalf("sectionMenuCursor after down = %d, want Memory index", m.sectionMenuCursor)
	}

	updated, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = updated.(Model)
	if m.sectionMenuOpen {
		t.Fatal("section menu stayed open after enter")
	}
	if m.currentSection != 2 {
		t.Fatalf("currentSection = %d, want Memory index", m.currentSection)
	}

	updated, _ = m.Update(keyPress("/"))
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m = updated.(Model)
	if m.sectionMenuOpen {
		t.Fatal("section menu stayed open after esc")
	}
}

func TestHelpAndQuitShortcutsUseControlKeys(t *testing.T) {
	m := NewModel()

	updated, cmd := m.Update(keyPress("h"))
	m = updated.(Model)
	if m.showHelp {
		t.Fatal("plain h opened help")
	}
	if cmd != nil {
		t.Fatal("plain h returned a command")
	}

	updated, cmd = m.Update(ctrlKey('h'))
	m = updated.(Model)
	if !m.showHelp {
		t.Fatal("ctrl+h did not open help")
	}
	if cmd != nil {
		t.Fatal("ctrl+h returned a command")
	}

	_, cmd = m.Update(keyPress("q"))
	if cmd != nil {
		t.Fatal("plain q returned a command")
	}

	_, cmd = m.Update(ctrlKey('q'))
	if cmd == nil {
		t.Fatal("ctrl+q did not return a quit command")
	}
}

func TestActivitySparklineAndCallFormatting(t *testing.T) {
	got := activitySparkline([]int64{0, 1, 2, 4}, 4)
	if got != " ⡀⡄⡇" {
		t.Fatalf("activitySparkline = %q, want %q", got, " ⡀⡄⡇")
	}
	for n, want := range map[int64]string{
		0:       "0 calls",
		1:       "1 call",
		999:     "999 calls",
		5200:    "5.2k calls",
		1200000: "1.2m calls",
	} {
		if got := formatActivityCalls(n); got != want {
			t.Fatalf("formatActivityCalls(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTokenSavingsBar(t *testing.T) {
	if got := tokenSavingsBar(913000, 16); got != "█████████░░░░░░░" {
		t.Fatalf("tokenSavingsBar = %q, want sample shape", got)
	}
	if got := tokenSavingsBar(0, 4); got != "░░░░" {
		t.Fatalf("tokenSavingsBar(0) = %q, want empty bar", got)
	}
}

func keyPress(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Text: s, Code: []rune(s)[0]})
}

func ctrlKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: r, Mod: tea.ModCtrl})
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
