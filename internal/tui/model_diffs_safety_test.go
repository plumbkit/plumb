package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/session"
)

// diffsHistoryReader builds a history.db holding items and opens it read-only.
func diffsHistoryReader(t *testing.T, items ...history.Item) *history.Reader {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "history.db")
	s, err := history.Open(dbPath, history.Options{})
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	for _, it := range items {
		s.Enqueue(it)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("history.Close: %v", err)
	}
	r, err := history.OpenReadOnlyAt(dbPath)
	if err != nil || r == nil {
		t.Fatalf("OpenReadOnlyAt: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func diffsItem(ws, name, before, after string, ms int64) history.Item {
	return history.Item{
		Change: history.Change{
			At: time.UnixMilli(ms), Op: history.OpUpdate, Kind: history.KindFile, Tool: "edit_file",
			Path:   filepath.Join(ws, name),
			Before: history.SideFromBytes([]byte(before)), After: history.SideFromBytes([]byte(after)),
		},
		Workspace: ws, CallID: "CALL_" + name, SessionID: "sess-1",
	}
}

func diffsModel(ws string, r *history.Reader) Model {
	return Model{
		width: 120, height: 40, leftWidth: 30,
		currentSection: 1, rightTab: 3, focusPanel: focusDiffs,
		sessions:      []session.Info{{Folder: ws}},
		historyReader: r,
	}
}

// File content and paths in history are untrusted: a repository can hold a
// file whose name or content carries an escape sequence. The tab and the detail
// overlay must display it, not let the terminal execute it.
func TestDiffsTerminalEscapesAreNeutralised(t *testing.T) {
	ws := t.TempDir()
	evilName := "evil\x1b]52;c;ZXZpbA==\x07.go"
	r := diffsHistoryReader(t, diffsItem(ws, evilName, "a\n", "a\nboom\x1b[2J\n", 1000))
	m := diffsModel(ws, r)

	m.refreshDiffs()
	if len(m.diffEntries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(m.diffEntries))
	}
	list := strings.Join(m.rightLinesDiffs(100), "\n")
	if strings.Contains(list, "\x1b]52") || strings.Contains(list, "\x07") {
		t.Fatalf("path escape sequence reached the list: %q", list)
	}
	if !strings.Contains(list, "^[]52") {
		t.Fatalf("expected the escape shown in caret notation, got: %q", list)
	}

	m.openDiffDetail(m.diffEntries[0])
	detail := strings.Join(m.diffDetailContentLines(100), "\n")
	if strings.Contains(detail, "\x1b[2J") || strings.Contains(detail, "\x1b]52") || strings.Contains(detail, "\x07") {
		t.Fatalf("escape sequence reached the detail overlay: %q", detail)
	}
	if !strings.Contains(detail, "boom^[[2J") {
		t.Fatalf("expected the diff's escape shown in caret notation, got: %q", detail)
	}
}

// A withheld row never shows content: not through the reader (openDiffDetail
// asks only for a recorded diff) and not through the body renderer.
func TestDiffDetailWithheldShowsMarkerOnly(t *testing.T) {
	ws := t.TempDir()
	r := diffsHistoryReader(t, diffsItem(ws, "notes.txt", "a\n", "a\nprivate words\n", 1000))
	m := diffsModel(ws, r)
	m.refreshDiffs()
	if len(m.diffEntries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(m.diffEntries))
	}

	// Control: as recorded (a diff) the content is shown.
	m.openDiffDetail(m.diffEntries[0])
	if !strings.Contains(m.diffDetailText, "private words") {
		t.Fatalf("control: expected the recorded diff, got %q", m.diffDetailText)
	}

	e := m.diffEntries[0]
	e.Content = history.ContentSensitive
	m.openDiffDetail(e)
	if m.diffDetailText != "" {
		t.Fatalf("a withheld row fetched its diff: %q", m.diffDetailText)
	}
	m.diffDetailText = "private words" // even if text were present, the body must not render it
	body := strings.Join(m.diffDetailBodyLines(), "\n")
	if strings.Contains(body, "private words") || !strings.Contains(body, "withheld") {
		t.Fatalf("withheld detail should show only the marker, got:\n%s", body)
	}
}

func TestDiffDetailOverlayQuitKeysAndMouse(t *testing.T) {
	m := Model{
		width: 100, height: 20, leftWidth: 30,
		currentSection: 1, rightTab: 3, diffDetailOpen: true,
		diffDetailEntry: history.Entry{Path: "a.go", Content: history.ContentDiff},
		diffDetailText:  strings.Repeat("+line\n", 200),
	}

	_, cmd := m.handleDiffDetailKey(tea.KeyPressMsg{Code: 'q', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+q in the diff overlay should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+q in the diff overlay should return tea.Quit")
	}
	if m2, _ := m.handleDiffDetailKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); !m2.waitingForQuit {
		t.Fatal("ctrl+c in the diff overlay should start the quit confirmation")
	}

	// The wheel scrolls the overlay, not the panel behind it.
	m.handleMouseWheel(tea.Mouse{X: 50, Y: 10}, 3)
	if m.diffDetailScroll != 3 || m.rightScroll != 0 {
		t.Fatalf("wheel: overlay scroll %d (want 3), panel scroll %d (want 0)", m.diffDetailScroll, m.rightScroll)
	}
	m.handleMouseWheel(tea.Mouse{X: 50, Y: 10}, -10)
	if m.diffDetailScroll != 0 {
		t.Fatalf("wheel up past the top should clamp to 0, got %d", m.diffDetailScroll)
	}
	// A click on the tab bar behind the overlay does nothing. Control: the
	// same click with the overlay closed does switch the tab.
	click := tea.Mouse{X: m.leftWidth + 3 + 5, Y: bodyStartRow, Button: tea.MouseLeft}
	m.handleLeftMouseClick(click)
	if m.rightTab != 3 {
		t.Fatalf("a click behind the overlay switched the tab to %d", m.rightTab)
	}
	m.diffDetailOpen = false
	m.handleLeftMouseClick(click)
	if m.rightTab != 0 {
		t.Fatalf("control: the tab-bar click should select tab 0 with the overlay closed, got %d", m.rightTab)
	}
}

// With no session or project folder an empty Workspace filter would list every
// workspace; the tab says so instead.
func TestDiffsNoWorkspaceShowsNothing(t *testing.T) {
	ws := t.TempDir()
	r := diffsHistoryReader(t, diffsItem(ws, "a.txt", "a\n", "b\n", 1000))
	m := diffsModel(ws, r)
	m.refreshDiffs()
	if len(m.diffEntries) != 1 { // control: the row exists and is listed with a workspace
		t.Fatalf("control: expected 1 entry, got %d", len(m.diffEntries))
	}

	m.sessions = nil
	m.refreshDiffs()
	if len(m.diffEntries) != 0 || !m.diffNoWorkspace {
		t.Fatalf("no workspace: entries %d, diffNoWorkspace %v", len(m.diffEntries), m.diffNoWorkspace)
	}
	if got := strings.Join(m.rightLinesDiffs(100), "\n"); !strings.Contains(got, "No workspace selected") {
		t.Fatalf("expected the no-workspace message, got:\n%s", got)
	}
}

func TestDiffsTabEntryLoadsAndCursorClampsToFilter(t *testing.T) {
	ws := t.TempDir()
	r := diffsHistoryReader(t,
		diffsItem(ws, "one.txt", "a\n", "b\n", 1000),
		diffsItem(ws, "two.txt", "a\n", "b\n", 2000),
		diffsItem(ws, "three.txt", "a\n", "b\n", 3000))

	// Tabbing onto Diffs loads it at once rather than on the next poll.
	m := diffsModel(ws, r)
	m.rightTab, m.focusPanel = 2, focusStats
	m = m.mainKeyTab()
	if m.rightTab != 3 || len(m.diffEntries) != 3 {
		t.Fatalf("tab onto Diffs: rightTab %d, entries %d (want 3, 3)", m.rightTab, len(m.diffEntries))
	}

	// The cursor clamps to the filtered list, not the unfiltered one.
	m.diffFilter = "two.txt"
	m.diffCursor = 2
	m.refreshDiffs()
	if m.diffCursor != 0 {
		t.Fatalf("cursor should clamp to the 1-row filtered list, got %d", m.diffCursor)
	}
}
