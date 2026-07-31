package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/plumbkit/plumb/internal/monitor"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

// mkCall builds a RecentCall with the given session id and a CalledAt
// derived from msOffset. Helper kept tiny so test intent stays obvious.
func mkCall(sess string, msOffset int64) stats.RecentCall {
	return stats.RecentCall{
		SessionID: sess,
		CalledAt:  time.UnixMilli(1_000_000_000_000 + msOffset),
	}
}

// The "uptime" anchor must follow the daemon, not the oldest live session, so it
// stays stable as conversations come and go while the daemon keeps running.
func TestDashboardUptimeStart(t *testing.T) {
	now := time.Unix(10_000, 0).UTC()
	daemonStart := now.Add(-3 * time.Hour)
	sessionStart := now.Add(-5 * time.Minute)

	// Fresh metrics snapshot: anchor on the real daemon start time, ignoring
	// the (younger) live session.
	m := Model{
		daemonMetricsOK: true,
		daemonMetrics:   monitor.DaemonMetrics{StartedAt: daemonStart},
		sessions:        []session.Info{{StartedAt: sessionStart}},
	}
	if got := m.dashboardUptimeStart(now); !got.Equal(daemonStart) {
		t.Fatalf("fresh metrics: got %v, want daemon start %v", got, daemonStart)
	}

	// No metrics (old daemon / stale snapshot): fall back to the oldest live session.
	m = Model{sessions: []session.Info{{StartedAt: sessionStart}}}
	if got := m.dashboardUptimeStart(now); !got.Equal(sessionStart) {
		t.Fatalf("no metrics: got %v, want session start %v", got, sessionStart)
	}

	// Metrics present but StartedAt zero (pre-upgrade daemon build): also fall back.
	m = Model{daemonMetricsOK: true, sessions: []session.Info{{StartedAt: sessionStart}}}
	if got := m.dashboardUptimeStart(now); !got.Equal(sessionStart) {
		t.Fatalf("zero StartedAt: got %v, want session start %v", got, sessionStart)
	}

	// Nothing at all: fall back to now-1m.
	m = Model{}
	if got := m.dashboardUptimeStart(now); !got.Equal(now.Add(-time.Minute)) {
		t.Fatalf("empty: got %v, want %v", got, now.Add(-time.Minute))
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
			{Name: "CRAZY-PLUMB", Language: "go", Folder: "."},
			{Name: "SUPER-FRIEND", Language: "go", Folder: "."},
		},
	}

	lines := m.leftLines()
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = ansiStripForTest(line)
	}
	joined := strings.Join(plain, "\n")
	for _, want := range []string{
		" ❯ CRAZY-PLUMB  GO ",
		"    ╰─ .",
		" ∙ SUPER-FRIEND  GO ",
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
		"╭─ Section ──────────╮ ╭─ Activity (1m) ───────╮",
		"│ ❯ 2. Sessions    ▽ │ │ ",
		"╰────────────────────╯ ╰───────────────────────╯",
	} {
		if !strings.HasPrefix(plain[i], want) {
			t.Fatalf("line %d = %q, want prefix %q", i, plain[i], want)
		}
	}
	if !strings.Contains(plain[1], "5.2k") {
		t.Fatalf("activity row = %q, want call count", plain[1])
	}
	if strings.Contains(plain[1], "calls") {
		t.Fatalf("activity row = %q, should not include calls label", plain[1])
	}
	if !strings.Contains(plain[1], "⣀") {
		t.Fatalf("activity row = %q, want faded baseline dots", plain[1])
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
	if !strings.Contains(plain[1], "■■■■■■■■") {
		t.Fatalf("daemon CPU row = %q, want segmented CPU bar", plain[1])
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
	if !strings.Contains(plain[0], "Token Efficiency") {
		t.Fatalf("top menu = %#v, want token efficiency box title", plain)
	}
	if !strings.Contains(plain[1], "913k") {
		t.Fatalf("token savings row = %q, want savings value", plain[1])
	}
	if !strings.Contains(plain[0], "╮ ╭─ Token Efficiency") {
		t.Fatalf("top menu = %q, want one-space widget gap", plain[0])
	}
}

func TestSectionMenuUsesNumberedRows(t *testing.T) {
	RebuildStyles()
	m := Model{sectionMenuCursor: 3}
	bg := strings.Repeat(strings.Repeat(" ", 40)+"\n", 8)
	plain := ansiStripForTest(m.renderSectionMenuOverlay(bg))
	for _, want := range []string{
		"╭─ Section ──────────╮",
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
			count := formatActivityCount(calls)
			wantSuffix := " " + count + " │"
			if !strings.HasSuffix(row, wantSuffix) {
				t.Fatalf("activity row = %q, want suffix %q", row, wantSuffix)
			}
			if strings.Contains(row, "  "+count+" │") {
				t.Fatalf("activity row = %q, want one space before count", row)
			}
			boxWidth := lipgloss.Width(m.renderActivityBox(false)[0])
			if lipgloss.Width(row) != boxWidth {
				t.Fatalf("activity row width = %d, want box width %d: %q", lipgloss.Width(row), boxWidth, row)
			}
		})
	}
}

func TestSectionSelectorKeyFlow(t *testing.T) {
	m := NewModel("", "")
	if m.currentSection != 0 {
		t.Fatalf("NewModel currentSection = %d, want Dashboard index", m.currentSection)
	}

	updated, _ := m.Update(keyPress("/"))
	m = updated.(Model)
	if !m.sectionMenuOpen {
		t.Fatal("section menu did not open")
	}
	if m.sectionMenuCursor != 0 {
		t.Fatalf("sectionMenuCursor = %d, want Dashboard index", m.sectionMenuCursor)
	}

	updated, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m = updated.(Model)
	if m.sectionMenuCursor != 1 {
		t.Fatalf("sectionMenuCursor after down = %d, want Sessions index", m.sectionMenuCursor)
	}

	updated, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = updated.(Model)
	if m.sectionMenuOpen {
		t.Fatal("section menu stayed open after enter")
	}
	if m.currentSection != 1 {
		t.Fatalf("currentSection = %d, want Sessions index", m.currentSection)
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
	if got := formatToolCallCount(2); got != "2 tool calls" {
		t.Fatalf("formatToolCallCount(2) = %q, want plural", got)
	}
}

func TestDiagnosticsControlOutputExplainsOldDaemon(t *testing.T) {
	got := diagnosticsControlOutput("error: unknown command \"diagnostics .\"\n")
	for _, want := range []string{"current daemon", "plumb stop"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnosticsControlOutput missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "unknown command") {
		t.Fatalf("diagnosticsControlOutput leaked raw control error: %q", got)
	}
}

func TestRender_AlignsBorders(t *testing.T) {
	RebuildStyles()
	m := NewModel("", "")
	m.width = 80
	m.height = 20
	m.ready = true
	m.currentSection = 1
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

	before, _, _ := strings.Cut(topBorder, "┬")
	topCharIdx := len([]rune(before))

	before, _, _ = strings.Cut(bodyLine, "┆")
	bodyCharIdx := len([]rune(before))

	bottomBorder := ansi.Strip(lines[18])
	before, _, _ = strings.Cut(bottomBorder, "┴")
	bottomCharIdx := len([]rune(before))

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
