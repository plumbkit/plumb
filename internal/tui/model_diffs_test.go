package tui

import (
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
