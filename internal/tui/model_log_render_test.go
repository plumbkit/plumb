// Log-view rendering tests: body-line padding and markers, the status bar,
// the logs frame, mouse/keyboard row selection, and the detail overlay.
// Split out of model_test.go, which had grown past the test-file size cap.
package tui

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

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

func TestRenderHelpGroupsShortcutsAndKeepsBorders(t *testing.T) {
	RebuildStyles()
	// Tall enough that the help box (one row per group + item) is not clipped.
	m := Model{width: 110, height: 36}
	bgLine := strings.Repeat(" ", m.width)
	bg := strings.TrimSuffix(strings.Repeat(bgLine+"\n", m.height), "\n")

	plain := ansiStripForTest(m.renderHelp(bg))
	for _, want := range []string{
		"Help & Navigation",
		"Navigation",
		"Sections",
		"Panels",
		"Sessions",
		"Rename the selected session",
		"Refresh sessions and stats",
		"Actions",
		"ctrl+1-5",
		"tab / shift+tab",
		"Switch focus: sessions, details, tools, recent",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("help overlay missing %q in:\n%s", want, plain)
		}
	}

	for line := range strings.SplitSeq(plain, "\n") {
		if !strings.Contains(line, "shift+tab") && !strings.Contains(line, "ctrl+1-5") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "│   ") {
			t.Fatalf("help row left padding = %q, want three spaces after border", line)
		}
		if !strings.HasSuffix(strings.TrimRight(line, " "), "│") {
			t.Fatalf("help row is missing right border: %q", line)
		}
		if !strings.HasSuffix(strings.TrimRight(line, "│"), "   ") {
			t.Fatalf("help row right padding = %q, want three spaces before border", line)
		}
	}
}

func TestSessionsRightFooterPinnedAndScrolls(t *testing.T) {
	RebuildStyles()
	const bodyHeight, rightWidth = 10, 40
	left := []string{"L0"}
	right := make([]string, 30) // overflows the viewport so the panel scrolls
	for i := range right {
		right[i] = fmt.Sprintf("content-%d", i)
	}

	render := func(section, scroll int) []string {
		m := Model{currentSection: section, leftWidth: 20, rightScroll: scroll}
		out := m.renderBodySection(left, right, bodyHeight, rightWidth, false)
		return strings.Split(ansiStripForTest(out), "\n")
	}

	// Sessions section: the hint sits on the last body row, with a blank
	// spacer directly above it.
	rows := render(1, 0)
	hint, spacer := rows[bodyHeight-1], rows[bodyHeight-2]
	if !strings.Contains(hint, "r rename") || !strings.Contains(hint, "a refresh") {
		t.Fatalf("footer hint missing from last row: %q", hint)
	}
	if strings.Contains(spacer, "rename") || strings.Contains(spacer, "refresh") {
		t.Fatalf("expected a blank spacer above the hint, got %q", spacer)
	}

	// Scrolled hard: the footer must stay put, and the scrollable region must
	// actually have advanced past the top.
	scrolled := render(1, 100)
	if !strings.Contains(scrolled[bodyHeight-1], "r rename") {
		t.Fatalf("footer not preserved when scrolled: %q", scrolled[bodyHeight-1])
	}
	if rows[0] == scrolled[0] {
		t.Fatalf("right panel did not scroll: top row unchanged (%q)", rows[0])
	}

	// Memory section (2) reserves no footer — the hint must not appear at all.
	for _, line := range render(2, 0) {
		if strings.Contains(line, "rename") || strings.Contains(line, "refresh") {
			t.Fatalf("Memory section must not show the sessions footer: %q", line)
		}
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
	raw := `time=2026-05-18T08:36:55.028+10:00 level=WARN msg="mcp: tool error" tool=read_file err="read_file: stat site/index.html: no such file or directory"`
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
