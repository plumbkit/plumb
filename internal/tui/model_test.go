package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/golimpio/plumb/internal/monitor"
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
		" ❯ CRAZY-PLUMB  go ",
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

func TestLeftPanelDoesNotShrinkBelowFullSessionRowWidth(t *testing.T) {
	m := Model{leftWidth: minLeftWidth + 2, width: 100, height: 30}

	updated, _ := m.Update(keyPress("["))
	m = updated.(Model)
	if m.leftWidth != minLeftWidth {
		t.Fatalf("leftWidth after key resize = %d, want %d", m.leftWidth, minLeftWidth)
	}

	m.setLeftWidthFromMouse(1)
	if m.leftWidth != minLeftWidth {
		t.Fatalf("leftWidth after mouse resize = %d, want %d", m.leftWidth, minLeftWidth)
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
		tokenSavings:    913000,
		daemonMetricsOK: true,
		daemonMetrics: monitor.DaemonMetrics{
			CPUPercent:   42.5,
			CPUAvailable: true,
		},
		daemonCPU: []float64{0, 5, 10, 20, 40, 60, 80, 100},
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
		"╭─ Section ──────────╮ ╭─ Activity (1m) ────────────╮",
		"│ ❯ 2. Sessions    ▽ │ │ ",
		"╰────────────────────╯ ╰────────────────────────────╯",
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
	if !strings.Contains(plain[0], "Daemon CPU") {
		t.Fatalf("top menu = %#v, want daemon CPU box title", plain)
	}
	if strings.Contains(plain[1], "RSS") || strings.Contains(plain[1], " H ") || strings.Contains(plain[1], " G ") {
		t.Fatalf("daemon CPU row = %q, should not show memory or goroutine labels", plain[1])
	}
	if !strings.Contains(plain[0], "42%") {
		t.Fatalf("daemon CPU title = %q, want CPU value in title", plain[0])
	}

	// Token savings box requires wide layouts: selector + activity + daemon CPU + token savings + gaps.
	lines = m.renderTopMenu(120, false)
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

func TestSectionMenuUsesNumberedRows(t *testing.T) {
	RebuildStyles()
	m := Model{sectionMenuCursor: 3}
	bg := strings.Repeat(strings.Repeat(" ", 40)+"\n", 8)
	plain := ansiStripForTest(m.renderSectionMenuOverlay(bg))
	for _, want := range []string{
		"╭────────────────────╮",
		"│   1. Dashboard     │",
		"│   2. Sessions      │",
		"│   3. Memory        │",
		"│ ❯ 4. Logs          │",
		"│   5. Settings      │",
		"╰────────────────────╯",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("section menu missing %q in:\n%s", want, plain)
		}
	}
}

func TestActivityBoxKeepsOneSpaceAfterCallCount(t *testing.T) {
	RebuildStyles()
	for _, calls := range []int64{2, 10, 500, 1300, 5200} {
		t.Run(formatActivityCalls(calls), func(t *testing.T) {
			m := Model{
				activity: stats.ActivitySummary{
					Calls:   calls,
					Buckets: []int64{0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
				},
			}

			row := ansiStripForTest(m.renderActivityBox(false)[1])
			count := formatActivityCalls(calls)
			wantSuffix := count + " │"
			if !strings.HasSuffix(row, wantSuffix) {
				t.Fatalf("activity row = %q, want suffix %q", row, wantSuffix)
			}
			if lipgloss.Width(row) != 30 {
				t.Fatalf("activity row width = %d, want 30: %q", lipgloss.Width(row), row)
			}
		})
	}
}

func TestSectionSelectorKeyFlow(t *testing.T) {
	m := NewModel("", "")

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

func TestSectionSelectorMouseAndControlNumber(t *testing.T) {
	m := Model{currentSection: 1, width: 100, height: 30}
	updated, _ := m.Update(tea.MouseClickMsg(tea.Mouse{X: 2, Y: 1, Button: tea.MouseLeft}))
	m = updated.(Model)
	if !m.sectionMenuOpen {
		t.Fatal("clicking selector did not open section menu")
	}

	updated, _ = m.Update(tea.MouseClickMsg(tea.Mouse{X: 3, Y: 4, Button: tea.MouseLeft}))
	m = updated.(Model)
	if m.sectionMenuOpen {
		t.Fatal("section menu stayed open after clicking a row")
	}
	if m.currentSection != 3 {
		t.Fatalf("currentSection after row click = %d, want Logs index", m.currentSection)
	}

	updated, _ = m.Update(keyPress("ctrl+1"))
	m = updated.(Model)
	if m.currentSection != 0 {
		t.Fatalf("currentSection after ctrl+1 = %d, want Dashboard index", m.currentSection)
	}

	updated, _ = m.Update(keyPress("/"))
	m = updated.(Model)
	updated, _ = m.Update(keyPress("5"))
	m = updated.(Model)
	if m.currentSection != 4 {
		t.Fatalf("currentSection after local 5 = %d, want Settings index", m.currentSection)
	}
}

func TestHelpAndQuitShortcutsUseControlKeys(t *testing.T) {
	m := NewModel("", "")

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

func TestFooterShowsLiveSessionsAndDaemonMem(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	m := NewModel("", "")
	if m.globalDB != nil {
		defer m.globalDB.Close()
	}
	m.width = 150
	m.height = 12

	plain := ansiStripForTest(m.render())
	for _, want := range []string{
		"no sessions",
		"daemon mem:",
		"/ menu",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("footer missing %q in:\n%s", want, plain)
		}
	}
}

func TestFooterCountFormatting(t *testing.T) {
	for _, tt := range []struct {
		n    int64
		want string
	}{
		{0, "no sessions"},
		{1, "1 session"},
		{3, "3 sessions"},
	} {
		if got := formatSessionCount(tt.n); got != tt.want {
			t.Fatalf("formatSessionCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
	if got := formatToolCallCount(1); got != "1 tool call" {
		t.Fatalf("formatToolCallCount(1) = %q, want singular", got)
	}
	if got := pluralWord(1, "token", "tokens"); got != "token" {
		t.Fatalf("pluralWord for one token = %q, want token", got)
	}
	if got := pluralWord(2, "token", "tokens"); got != "tokens" {
		t.Fatalf("pluralWord for two tokens = %q, want tokens", got)
	}
}

func TestLogBodyLineKeepsPadBeforeRightBorder(t *testing.T) {
	RebuildStyles()
	m := Model{}
	entry := logEntry{Raw: "line 309 char abcdefghijklmnopqrstuvwxyz"}
	got := ansiStripForTest(m.renderLogBodyLine(&entry, 22, false, false, SepStyle.Render("│")))
	if !strings.HasSuffix(got, " │") {
		t.Fatalf("log body line = %q, want a space before right border", got)
	}
	if lipgloss.Width(got) != 24 {
		t.Fatalf("log body line width = %d, want 24: %q", lipgloss.Width(got), got)
	}
	if !strings.HasPrefix(got, "│ ") {
		t.Fatalf("log body line = %q, want a space after left border", got)
	}
}

func TestLogBodyLineUsesMarkersAndSelectedForegroundOnly(t *testing.T) {
	RebuildStyles()
	m := Model{}
	entry := logEntry{Raw: "short"}

	plain := ansiStripForTest(m.renderLogBodyLine(&entry, 30, false, false, SepStyle.Render("│")))
	if !strings.HasPrefix(plain, "│ • short") {
		t.Fatalf("log body line = %q, want bullet marker with one-cell left padding", plain)
	}

	selected := m.renderLogBodyLine(&entry, 30, true, false, SepStyle.Render("│"))
	selectedPlain := ansiStripForTest(selected)
	if !strings.HasPrefix(selectedPlain, "│ ❯ short") {
		t.Fatalf("selected log body line = %q, want selected marker with one-cell left padding", selectedPlain)
	}
	if strings.Contains(selected, "\x1b[48;") {
		t.Fatalf("selected log row should not use a background escape: %q", selected)
	}
	if !strings.Contains(selected, "\x1b[") {
		t.Fatalf("selected log row missing foreground styling: %q", selected)
	}
	if lipgloss.Width(selected) != 32 {
		t.Fatalf("selected log row width = %d, want 32", lipgloss.Width(selected))
	}
}

func TestLogStatusBarUsesInFrameText(t *testing.T) {
	RebuildStyles()
	m := Model{logEntries: []logEntry{{Raw: "one"}, {Raw: "two"}}}
	got := ansiStripForTest(m.renderLogStatusBar(m.logEntries, 58, false))
	for _, want := range []string{"Type to filter", "enter details", "2/2 lines"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log status missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "backspace erase") {
		t.Fatalf("log status still contains removed hint: %q", got)
	}
	if !strings.HasPrefix(got, "│  ") || !strings.HasSuffix(got, "  │") {
		t.Fatalf("log status = %q, want frame gap plus status text padding", got)
	}
}

func TestLogsTopBorderUsesPlainLogoIntegratedFrame(t *testing.T) {
	RebuildStyles()
	m := Model{width: 80, logEntries: []logEntry{{Raw: "one"}}, logFilter: "one"}
	got := ansiStripForTest(m.renderTopBorderLogs(false))
	for _, unwanted := range []string{"Logs", "Filter:", "lines"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("logs top border contains %q: %q", unwanted, got)
		}
	}
	if !strings.Contains(got, "╰╯ ╭") {
		t.Fatalf("logs top border does not include logo bottom join: %q", got)
	}
	if !strings.HasPrefix(got, "╭") {
		t.Fatalf("logs top border = %q, want top-left corner", got)
	}
	if !utf8.ValidString(got) || strings.ContainsRune(got, '�') {
		t.Fatalf("logs top border contains broken UTF-8: %q", got)
	}
}

func TestDashboardTopBorderUsesPlainLogoIntegratedFrame(t *testing.T) {
	RebuildStyles()
	m := Model{currentSection: 0, width: 100, height: 12, ready: true}

	lines := strings.Split(ansiStripForTest(m.renderDashboard()), "\n")
	if len(lines) < 4 {
		t.Fatalf("dashboard rendered too few lines: %#v", lines)
	}
	got := lines[3]
	if strings.Contains(got, "Dashboard") {
		t.Fatalf("dashboard top border contains title text: %q", got)
	}
	if !strings.Contains(got, "╰╯ ╭") {
		t.Fatalf("dashboard top border does not include logo bottom join: %q", got)
	}
	if !utf8.ValidString(got) || strings.ContainsRune(got, '�') {
		t.Fatalf("dashboard top border contains broken UTF-8: %q", got)
	}
}

func TestLogsSectionKeepsUniversalStatusBar(t *testing.T) {
	RebuildStyles()
	m := Model{
		currentSection: 3,
		width:          120,
		height:         14,
		logEntries:     []logEntry{{Raw: "one"}, {Raw: "two"}},
	}
	plain := ansiStripForTest(m.renderLogsSection())
	for _, want := range []string{
		"Type to filter",
		"enter details",
		"plumb dev",
		"/ menu",
		"^q quit",
		"^h help",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("logs section missing %q in:\n%s", want, plain)
		}
	}
	if got := len(strings.Split(plain, "\n")); got != m.height {
		t.Fatalf("logs section rendered %d rows, want %d:\n%s", got, m.height, plain)
	}
}

func TestLogMouseClickAndWheelSelectRows(t *testing.T) {
	m := Model{
		currentSection: 3,
		width:          80,
		height:         12,
		logEntries: []logEntry{
			{Raw: "one"},
			{Raw: "two"},
			{Raw: "three"},
			{Raw: "four"},
		},
	}

	updated, _ := m.Update(tea.MouseClickMsg(tea.Mouse{X: 4, Y: bodyStartRow + 2, Button: tea.MouseLeft}))
	m = updated.(Model)
	if m.logCursor != 2 {
		t.Fatalf("logCursor after click = %d, want 2", m.logCursor)
	}

	updated, _ = m.Update(tea.MouseWheelMsg(tea.Mouse{X: 4, Y: bodyStartRow + 2, Button: tea.MouseWheelUp}))
	m = updated.(Model)
	if m.logCursor != 0 {
		t.Fatalf("logCursor after wheel up = %d, want 0", m.logCursor)
	}

	updated, _ = m.Update(tea.MouseWheelMsg(tea.Mouse{X: 4, Y: bodyStartRow + 2, Button: tea.MouseWheelDown}))
	m = updated.(Model)
	if m.logCursor != 3 {
		t.Fatalf("logCursor after wheel down = %d, want 3", m.logCursor)
	}
}

func TestLogEnterOpensDetail(t *testing.T) {
	m := Model{
		currentSection: 3,
		width:          80,
		height:         12,
		logEntries:     []logEntry{{Raw: "one"}},
	}
	updated, _ := m.Update(keyPress("enter"))
	m = updated.(Model)
	if !m.logDetailOpen {
		t.Fatal("enter did not open log detail")
	}
	updated, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m = updated.(Model)
	if m.logDetailOpen {
		t.Fatal("esc did not close log detail")
	}
}

func TestLogDetailCopyShortcutReturnsCommand(t *testing.T) {
	m := Model{
		currentSection: 3,
		width:          80,
		height:         12,
		logEntries:     []logEntry{{Raw: "one"}},
		logDetailOpen:  true,
	}
	updated, cmd := m.Update(keyPress("c"))
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("c did not return a copy command")
	}
	if !m.logDetailCopied {
		t.Fatal("c did not set copied status")
	}
	updated, _ = m.Update(logDetailCopyResetMsg{})
	m = updated.(Model)
	if m.logDetailCopied {
		t.Fatal("copy reset did not restore status")
	}
}

func TestCurrentLogDetailTextReturnsRawLine(t *testing.T) {
	raw := `time=2026-05-18T08:36:55.028+10:00 level=WARN msg="mcp: tool error" tool=read_file err="full raw value"`
	m := Model{
		logEntries: []logEntry{{Raw: raw}},
	}
	if got := m.currentLogDetailText(); got != raw+"\n" {
		t.Fatalf("currentLogDetailText = %q, want raw line", got)
	}
}

func TestLogDetailFormatsTextSlogFields(t *testing.T) {
	raw := `time=2026-05-18T12:34:56Z level=INFO msg="daemon: ready" socket=/tmp/plumb.sock pid=123`
	lines := ansiStripForTest(strings.Join(logDetailLines(logEntry{Raw: raw}, 80), "\n"))
	for _, want := range []string{
		"Time     2026-05-18T12:34:56Z",
		"Level    INFO",
		"Message  daemon: ready",
		"┊ pid=123",
		"┊ socket=/tmp/plumb.sock",
		"Raw",
		`┊ time=2026-05-18T12:34:56Z level=INFO msg="daemon: ready" socket=/tmp/plumb.soc`,
		"┊ pid=123",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("log detail missing %q in:\n%s", want, lines)
		}
	}
}

func TestLogDetailFrameHasStatusBarAndFixedBlankRows(t *testing.T) {
	RebuildStyles()
	m := Model{
		width:           100,
		height:          20,
		logDetailScroll: 0,
	}
	bg := strings.Repeat(strings.Repeat(" ", 100)+"\n", 19) + strings.Repeat(" ", 100)
	got := ansiStripForTest(m.renderLogDetail(bg, []logEntry{{Raw: "line"}}))
	if strings.Contains(got, "esc close ─╮") {
		t.Fatalf("log detail top border still contains close hint:\n%s", got)
	}
	for _, want := range []string{"Log Detail", "c copy", "esc close"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log detail missing %q:\n%s", want, got)
		}
	}
	lines := strings.Split(got, "\n")
	for i, line := range lines {
		if strings.Contains(line, "Log Detail") {
			if i+1 >= len(lines) || strings.Trim(lines[i+1], " │") != "" {
				t.Fatalf("line after top border should be blank:\n%s", got)
			}
			break
		}
	}
	if !strings.HasPrefix(lines[bodyStartRow], "╭") || !strings.HasSuffix(lines[bodyStartRow], "╮") {
		t.Fatalf("log detail top border is not full-width at row %d:\n%s", bodyStartRow, got)
	}
	if !strings.HasPrefix(lines[m.height-2], "╰") || !strings.HasSuffix(lines[m.height-2], "╯") {
		t.Fatalf("log detail bottom border is not aligned with sessions popup:\n%s", got)
	}
}

func TestLogDetailContentUsesTwoSpacePadding(t *testing.T) {
	RebuildStyles()
	m := Model{width: 100, height: 20}
	bg := strings.Repeat(strings.Repeat(" ", 100)+"\n", 19) + strings.Repeat(" ", 100)
	got := ansiStripForTest(m.renderLogDetail(bg, []logEntry{{Raw: `time=2026-05-18T12:34:56Z level=INFO msg="daemon: ready"`}}))
	if !strings.Contains(got, "│  Time     2026-05-18T12:34:56Z") {
		t.Fatalf("log detail content does not use two-space left padding:\n%s", got)
	}
	if strings.Contains(got, "│  c copy") {
		t.Fatalf("log detail status bar should keep one-space padding:\n%s", got)
	}
}

func TestLogDetailStatusShowsCopiedMessage(t *testing.T) {
	RebuildStyles()
	m := Model{logDetailCopied: true}
	got := ansiStripForTest(m.renderLogDetailStatusBar(50))
	if !strings.Contains(got, "Copied to the clipboard") {
		t.Fatalf("copied status missing:\n%s", got)
	}
	if strings.Contains(got, "c copy") {
		t.Fatalf("copied status should replace normal text:\n%s", got)
	}
}

func TestLogDetailRawWrapsWithoutEllipsis(t *testing.T) {
	RebuildStyles()
	raw := `time=2026-05-18T08:36:55.028+10:00 level=WARN msg="mcp: tool error" tool=read_file err="read_file: stat /Users/gilberto/Projects/plumb/site/index.html: no such file or directory"`
	lines := ansiStripForTest(strings.Join(logDetailLines(logEntry{Raw: raw}, 64), "\n"))
	if strings.Contains(lines, "…") {
		t.Fatalf("raw log detail should wrap without ellipsis:\n%s", lines)
	}
	for _, want := range []string{"tool=read_file", "no such file or directory"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("raw log detail missing %q:\n%s", want, lines)
		}
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

func TestCPUSparklineUsesFixedPercentScale(t *testing.T) {
	got := cpuSparkline([]float64{0, 25, 50, 75, 100, 150}, 6)
	if got != " ⡄⡇⣧⣿⣿" {
		t.Fatalf("cpuSparkline = %q, want fixed 0-100%% scale", got)
	}
	if got := cpuSparkline(nil, 4); got != "    " {
		t.Fatalf("cpuSparkline(nil) = %q, want blank sparkline", got)
	}
}

func TestTokenSavingsBar(t *testing.T) {
	if filled, unfilled := tokenSavingsBar(913000, 16); filled+unfilled != "█████████░░░░░░░" {
		t.Fatalf("tokenSavingsBar = %q+%q, want sample shape", filled, unfilled)
	}
	if filled, unfilled := tokenSavingsBar(0, 4); filled+unfilled != "░░░░" {
		t.Fatalf("tokenSavingsBar(0) = %q+%q, want empty bar", filled, unfilled)
	}
}

func TestDiagnosticsControlOutputExplainsOldDaemon(t *testing.T) {
	got := diagnosticsControlOutput("error: unknown command \"diagnostics /Users/gilberto/Projects/plumb\"\n")
	for _, want := range []string{"current daemon", "plumb stop"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnosticsControlOutput missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "unknown command") {
		t.Fatalf("diagnosticsControlOutput leaked raw control error: %q", got)
	}
}

func keyPress(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Text: s, Code: []rune(s)[0]})
}

func ctrlKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: r, Mod: tea.ModCtrl})
}

func TestRender_AlignsBorders(t *testing.T) {
	RebuildStyles()
	m := NewModel("", "")
	m.width = 80
	m.height = 20
	m.ready = true
	m.leftWidth = minLeftWidth
	m.sessions = []session.Info{
		{ID: "s1", Name: "VERY-LONG-SESSION-NAME-THAT-EXCEEDS-WIDTH", Folder: "/tmp"},
	}

	out := m.render()
	lines := strings.Split(out, "\n")

	// Top border is at line 4 (index 3)
	topBorder := ansi.Strip(lines[3])
	// Body starts at line 5 (index 4)
	// Line 4 is Sessions (1) title
	// Line 5 is empty
	// Line 6 is the long session name
	bodyLine := ansi.Strip(lines[6])

	topIdx := strings.Index(topBorder, "┬")
	bodyIdx := strings.Index(bodyLine, "┆")

	topCharIdx := len([]rune(topBorder[:topIdx]))
	bodyCharIdx := len([]rune(bodyLine[:bodyIdx]))

	bottomBorder := ansi.Strip(lines[18])
	bottomIdx := strings.Index(bottomBorder, "┴")
	bottomCharIdx := len([]rune(bottomBorder[:bottomIdx]))

	if topCharIdx != bodyCharIdx || topCharIdx != bottomCharIdx {
		t.Errorf("Misalignment: top connector at char %d, body divider at char %d, bottom connector at char %d\ntop:    %s\nbody:   %s\nbottom: %s", topCharIdx, bodyCharIdx, bottomCharIdx, topBorder, bodyLine, bottomBorder)
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
