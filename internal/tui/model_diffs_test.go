package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/stats"
)

func TestDiffsTabNavigation(t *testing.T) {
	m := Model{
		currentSection: 1, // Sessions
		rightTab:       2, // History
		focusPanel:     focusStats,
	}

	// Tab advances to Diffs (3)
	m = m.mainKeyTab()
	if m.rightTab != 3 {
		t.Fatalf("rightTab after Tab = %d, want 3 (Diffs)", m.rightTab)
	}
	if m.focusPanel != focusDiffs {
		t.Fatalf("focusPanel = %v, want focusDiffs", m.focusPanel)
	}

	// Tab advances to Diagnostics (4)
	m = m.mainKeyTab()
	if m.rightTab != 4 {
		t.Fatalf("rightTab after Tab = %d, want 4 (Diagnostics)", m.rightTab)
	}
	if m.focusPanel != focusDiagnostics {
		t.Fatalf("focusPanel = %v, want focusDiagnostics", m.focusPanel)
	}

	// Shift+Tab steps back to Diffs (3)
	m = m.mainKeyShiftTab()
	if m.rightTab != 3 {
		t.Fatalf("rightTab after Shift+Tab = %d, want 3 (Diffs)", m.rightTab)
	}
	if m.focusPanel != focusDiffs {
		t.Fatalf("focusPanel = %v, want focusDiffs", m.focusPanel)
	}

	// Mouse click on Diffs pill
	m.rightTab = 0
	m.handleTabBarClick(m.leftWidth + 3 + 40) // within relX 35..45
	if m.rightTab != 3 || m.focusPanel != focusDiffs {
		t.Fatalf("click at relX 40: rightTab=%d focusPanel=%v, want 3 and focusDiffs", m.rightTab, m.focusPanel)
	}
}

func TestDiffsListRenderingAndGaps(t *testing.T) {
	entries := []history.Entry{
		{
			Seq:          101,
			At:           time.Date(2026, 10, 7, 12, 0, 0, 0, time.Local),
			Tool:         "edit_file",
			Op:           history.OpUpdate,
			SessionName:  "bright-hawk",
			LogicalAgent: "worker-1",
			Path:         "internal/tui/model.go",
			Added:        12,
			Removed:      3,
			Content:      history.ContentDiff,
			GapBefore:    true,
			GapDropped:   true,
		},
		{
			Seq:          102,
			At:           time.Date(2026, 10, 7, 12, 1, 0, 0, time.Local),
			Tool:         "write_file",
			Op:           history.OpCreate,
			SessionName:  "bright-hawk",
			LogicalAgent: "worker-1",
			Path:         "secrets/.env",
			Added:        5,
			Removed:      0,
			Content:      history.ContentSensitive,
		},
	}

	m := &Model{
		currentSection: 1,
		rightTab:       3,
		focusPanel:     focusDiffs,
		diffEntries:    entries,
		diffCursor:     0,
		historyReader:  &history.Reader{}, // non-nil sentinel
	}

	lines := m.rightLinesDiffs(100)
	joined := strings.Join(lines, "\n")

	// Header row present
	if !strings.Contains(joined, "When") || !strings.Contains(joined, "Op") || !strings.Contains(joined, "Tool") {
		t.Fatalf("expected header columns in diffs list, got:\n%s", joined)
	}

	// Gap row rendered with dropped notice
	if !strings.Contains(joined, "unrecorded change") {
		t.Fatalf("expected unrecorded change gap line, got:\n%s", joined)
	}
	if !strings.Contains(joined, "history dropped rows in this interval") {
		t.Fatalf("expected dropped rows notice in gap line, got:\n%s", joined)
	}

	// Withheld row rendered as marker, not content
	if !strings.Contains(joined, "[sensitive]") {
		t.Fatalf("expected [sensitive] content marker for withheld row, got:\n%s", joined)
	}
}

func TestDiffsFilter(t *testing.T) {
	entries := []history.Entry{
		{Seq: 1, Tool: "edit_file", Path: "pkg/one.go", SessionName: "sesh-1", LogicalAgent: "worker"},
		{Seq: 2, Tool: "write_file", Path: "pkg/two.go", SessionName: "sesh-2", LogicalAgent: "reviewer"},
		{Seq: 3, Tool: "delete_file", Path: "pkg/three.go", SessionName: "sesh-3", LogicalAgent: "worker"},
	}

	m := Model{
		diffEntries: entries,
	}

	// Substring filter matching tool
	m.diffFilter = "write"
	filtered := m.filteredDiffEntries()
	if len(filtered) != 1 || filtered[0].Seq != 2 {
		t.Fatalf("filter 'write' got %d entries, want [Seq 2]", len(filtered))
	}

	// Substring filter matching agent
	m.diffFilter = "reviewer"
	filtered = m.filteredDiffEntries()
	if len(filtered) != 1 || filtered[0].Seq != 2 {
		t.Fatalf("filter 'reviewer' got %d entries, want [Seq 2]", len(filtered))
	}

	// Substring filter matching path
	m.diffFilter = "three"
	filtered = m.filteredDiffEntries()
	if len(filtered) != 1 || filtered[0].Seq != 3 {
		t.Fatalf("filter 'three' got %d entries, want [Seq 3]", len(filtered))
	}

	// Filter key handling
	m.diffFilter = ""
	m.diffFilterActive = true
	m, handled := m.handleDiffFilterKey("e")
	if !handled || m.diffFilter != "e" {
		t.Fatalf("typing 'e' handled=%v diffFilter=%q", handled, m.diffFilter)
	}
	m, handled = m.handleDiffFilterKey("backspace")
	if !handled || m.diffFilter != "" {
		t.Fatalf("backspace handled=%v diffFilter=%q", handled, m.diffFilter)
	}
	m.diffFilter = "applied"
	m, _ = m.handleDiffFilterKey("enter")
	if m.diffFilterActive || m.diffFilter != "applied" {
		t.Fatalf("enter did not close active editing while keeping query: active=%v query=%q", m.diffFilterActive, m.diffFilter)
	}
	m, _ = m.handleDiffFilterKey("esc")
	if m.diffFilter != "" {
		t.Fatalf("esc did not clear filter query: got %q", m.diffFilter)
	}
}

func TestDiffDetailOverlay(t *testing.T) {
	entry := history.Entry{
		Seq:          42,
		At:           time.Date(2026, 10, 7, 14, 0, 0, 0, time.Local),
		CallID:       "01CALL123",
		Tool:         "edit_file",
		Op:           history.OpUpdate,
		SessionName:  "swift-otter",
		LogicalAgent: "atlas",
		Path:         "main.go",
		Added:        3,
		Removed:      1,
		Content:      history.ContentDiff,
	}

	m := Model{
		width:              100,
		height:             30,
		diffDetailOpen:     true,
		diffDetailEntry:    entry,
		diffDetailText:     "@@ -1,3 +1,5 @@\n-old line\n+new line\n context line\n",
		diffDetailHaveCall: true,
		diffDetailCall: stats.CallSummary{
			Tool:        "edit_file",
			DurationMs:  18,
			Success:     true,
			SessionName: "swift-otter",
			CalledAt:    time.Date(2026, 10, 7, 14, 0, 0, 0, time.Local),
		},
	}

	lines := m.diffDetailContentLines(90)
	joined := strings.Join(lines, "\n")

	// Call summary metadata
	if !strings.Contains(joined, "Tool Call:") || !strings.Contains(joined, "18ms") || !strings.Contains(joined, "success") {
		t.Fatalf("expected tool call summary in diff detail, got:\n%s", joined)
	}

	// Change metadata
	if !strings.Contains(joined, "seq #42") || !strings.Contains(joined, "+3 -1") {
		t.Fatalf("expected change metadata in diff detail, got:\n%s", joined)
	}

	// Unified diff lines
	if !strings.Contains(joined, "--- a/main.go") || !strings.Contains(joined, "+++ b/main.go") {
		t.Fatalf("expected diff headers in diff detail, got:\n%s", joined)
	}
	if !strings.Contains(joined, "+new line") || !strings.Contains(joined, "-old line") {
		t.Fatalf("expected diff content in diff detail, got:\n%s", joined)
	}

	// Key handling closes detail on esc / q / enter
	for _, key := range []string{"esc", "q", "enter"} {
		m.diffDetailOpen = true
		m, _ = m.handleDiffDetailKey(tea.KeyPressMsg{Code: 0, Text: key})
		if m.diffDetailOpen {
			t.Fatalf("key %q did not close diff detail overlay", key)
		}
	}
}

func TestDiffsClickRowMapping(t *testing.T) {
	entries := []history.Entry{
		{Seq: 1, Tool: "edit_file", Path: "pkg/one.go", GapBefore: true},
		{Seq: 2, Tool: "write_file", Path: "pkg/two.go", GapBefore: false},
		{Seq: 3, Tool: "delete_file", Path: "pkg/three.go", GapBefore: false},
	}
	m := &Model{
		currentSection: 1,
		rightTab:       3, // Diffs tab
		focusPanel:     focusDiffs,
		diffEntries:    entries,
		diffCursor:     0,
		historyReader:  &history.Reader{},
	}

	// Layout in rightLines:
	// 0: tab bar
	// 1: blank
	// 2: table header
	// 3: separator
	// 4: entry 0 (pkg/one.go)
	// 5: entry 0 gap (GapBefore)
	// 6: entry 1 (pkg/two.go)
	// 7: entry 2 (pkg/three.go)

	// Header and separator clicks must be non-selecting
	if idx := m.diffEntryAtLine(2); idx != -1 {
		t.Fatalf("click at header row 2 got %d, want -1", idx)
	}
	if idx := m.diffEntryAtLine(3); idx != -1 {
		t.Fatalf("click at separator row 3 got %d, want -1", idx)
	}

	// Entry 0 row click
	if idx := m.diffEntryAtLine(4); idx != 0 {
		t.Fatalf("click at row 4 got %d, want 0", idx)
	}

	// Gap row click must be non-selecting
	if idx := m.diffEntryAtLine(5); idx != -1 {
		t.Fatalf("click at gap row 5 got %d, want -1", idx)
	}

	// Entry 1 row click
	if idx := m.diffEntryAtLine(6); idx != 1 {
		t.Fatalf("click at row 6 got %d, want 1", idx)
	}

	// Entry 2 row click
	if idx := m.diffEntryAtLine(7); idx != 2 {
		t.Fatalf("click at row 7 got %d, want 2", idx)
	}

	// Out of bounds click
	if idx := m.diffEntryAtLine(8); idx != -1 {
		t.Fatalf("click at row 8 got %d, want -1", idx)
	}

	// Verify handleRightPanelClick uses mapping and ignores gaps/headers
	m.handleRightPanelClick(2) // header
	if m.diffCursor != 0 {
		t.Fatalf("header click changed diffCursor to %d", m.diffCursor)
	}

	m.handleRightPanelClick(5) // gap
	if m.diffCursor != 0 {
		t.Fatalf("gap click changed diffCursor to %d", m.diffCursor)
	}

	m.handleRightPanelClick(6) // entry 1
	if m.diffCursor != 1 {
		t.Fatalf("entry 1 click got diffCursor %d, want 1", m.diffCursor)
	}
}

func TestDiffsCursorViewportScroll(t *testing.T) {
	entries := make([]history.Entry, 0, 40)
	for i := range 40 {
		entries = append(entries, history.Entry{
			Seq:  int64(i + 1),
			Tool: "edit_file",
			Path: fmt.Sprintf("file_%02d.go", i),
		})
	}

	m := Model{
		height:         20,
		width:          80,
		currentSection: 1,
		rightTab:       3,
		focusPanel:     focusDiffs,
		diffEntries:    entries,
		diffCursor:     0,
		rightScroll:    0,
		historyReader:  &history.Reader{},
	}

	// 25 Down presses
	for range 25 {
		m = m.mainKeyDown()
	}

	if m.diffCursor != 25 {
		t.Fatalf("diffCursor after 25 Down presses = %d, want 25", m.diffCursor)
	}

	// Entry 25 line must be visible in viewport
	entryLine := m.diffEntryLine(m.diffCursor)
	rightViewH := max(max(m.height-6, 1)-2, 1) // footer has 2 lines in section 1
	if entryLine < m.rightScroll || entryLine >= m.rightScroll+rightViewH {
		t.Fatalf("entry line %d not in viewport [%d, %d)", entryLine, m.rightScroll, m.rightScroll+rightViewH)
	}
	if m.rightScroll == 0 {
		t.Fatalf("expected rightScroll > 0 after 25 Down presses, got 0")
	}

	// 25 Up presses should return to top and scroll to 0
	for range 25 {
		m = m.mainKeyUp()
	}
	if m.diffCursor != 0 {
		t.Fatalf("diffCursor after 25 Up presses = %d, want 0", m.diffCursor)
	}
	if m.rightScroll != 0 {
		t.Fatalf("rightScroll after returning to top = %d, want 0", m.rightScroll)
	}
}

func TestDiffDetailScrollBounds(t *testing.T) {
	entry := history.Entry{
		Seq:     1,
		Tool:    "edit_file",
		Path:    "large.go",
		Content: history.ContentDiff,
	}
	diffLines := make([]string, 50)
	for i := range diffLines {
		diffLines[i] = fmt.Sprintf("+line %d", i)
	}
	m := Model{
		width:           80,
		height:          24,
		diffDetailOpen:  true,
		diffDetailEntry: entry,
		diffDetailText:  strings.Join(diffLines, "\n"),
	}

	maxScroll := m.diffDetailMaxScroll()
	if maxScroll <= 0 {
		t.Fatalf("expected non-zero maxScroll for 50-line diff in 24-row terminal, got %d", maxScroll)
	}

	// Press G to jump to bottom
	m, _ = m.handleDiffDetailKey(tea.KeyPressMsg{Code: 0, Text: "G"})
	if m.diffDetailScroll != maxScroll {
		t.Fatalf("after G, scroll = %d, want maxScroll %d", m.diffDetailScroll, maxScroll)
	}

	// Press k once to step up one row
	m, _ = m.handleDiffDetailKey(tea.KeyPressMsg{Code: 0, Text: "k"})
	if m.diffDetailScroll != maxScroll-1 {
		t.Fatalf("after k, scroll = %d, want %d", m.diffDetailScroll, maxScroll-1)
	}

	// Press j beyond end should not overscroll
	m, _ = m.handleDiffDetailKey(tea.KeyPressMsg{Code: 0, Text: "G"})
	m, _ = m.handleDiffDetailKey(tea.KeyPressMsg{Code: 0, Text: "j"})
	if m.diffDetailScroll != maxScroll {
		t.Fatalf("after j past bottom, scroll = %d, want %d", m.diffDetailScroll, maxScroll)
	}
}
