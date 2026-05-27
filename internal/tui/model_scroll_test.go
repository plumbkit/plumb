package tui

import (
	"testing"

	"github.com/golimpio/plumb/internal/memory"
	"github.com/golimpio/plumb/internal/session"
)

// mkSessions builds n placeholder sessions with empty Folder so refreshStats
// stays cheap (no per-folder topology/diagnostics work).
func mkSessions(n int) []session.Info {
	s := make([]session.Info, n)
	for i := range s {
		s[i] = session.Info{ID: string(rune('a' + i%26))}
	}
	return s
}

func mkMemories(n int) []memory.Memory {
	m := make([]memory.Memory, n)
	for i := range m {
		m[i] = memory.Memory{Name: string(rune('a' + i%26))}
	}
	return m
}

// assertLeftCursorVisible checks the invariant ensureLeftCursorVisible must
// preserve: the selected row's content lines lie within the body viewport.
func assertLeftCursorVisible(t *testing.T, m Model) {
	t.Helper()
	const headerLines = 2
	cursor, perItem := m.cursor, 3
	if m.currentSection == 2 {
		cursor, perItem = m.memoryCursor, 4
	}
	top := headerLines + cursor*perItem
	bottom := top + perItem - 2
	bodyHeight := max(m.height-6, 1)
	if top < m.leftScroll || bottom >= m.leftScroll+bodyHeight {
		t.Fatalf("selection row [%d,%d] not within viewport [%d,%d) (cursor=%d, leftScroll=%d)",
			top, bottom, m.leftScroll, m.leftScroll+bodyHeight, cursor, m.leftScroll)
	}
}

func TestEnsureLeftCursorVisible(t *testing.T) {
	for _, tt := range []struct {
		name           string
		section        int
		sessions       int
		memories       int
		cursor         int
		startScroll    int
		height         int // bodyHeight = height-6
		wantLeftScroll int
	}{
		{
			name:    "sessions cursor below viewport scrolls down",
			section: 1, sessions: 20, cursor: 10, startScroll: 0, height: 14, wantLeftScroll: 26,
		},
		{
			name:    "sessions cursor above viewport scrolls up and reveals header",
			section: 1, sessions: 20, cursor: 0, startScroll: 26, height: 14, wantLeftScroll: 0,
		},
		{
			name:    "sessions cursor already visible does not move viewport",
			section: 1, sessions: 20, cursor: 8, startScroll: 24, height: 14, wantLeftScroll: 24,
		},
		{
			name:    "sessions last item clamps within bounds",
			section: 1, sessions: 20, cursor: 19, startScroll: 0, height: 14, wantLeftScroll: 53,
		},
		{
			name:    "memory perItem=4 boundary scrolls down",
			section: 2, memories: 10, cursor: 5, startScroll: 0, height: 12, wantLeftScroll: 19,
		},
		{
			name:    "memory first item reveals header",
			section: 2, memories: 10, cursor: 0, startScroll: 30, height: 12, wantLeftScroll: 0,
		},
		{
			name:    "empty sessions resets scroll and never panics",
			section: 1, sessions: 0, cursor: -1, startScroll: 9, height: 14, wantLeftScroll: 0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				currentSection: tt.section,
				height:         tt.height,
				leftScroll:     tt.startScroll,
				sessions:       mkSessions(tt.sessions),
				memories:       mkMemories(tt.memories),
			}
			if tt.section == 2 {
				m.memoryCursor = tt.cursor
			} else {
				m.cursor = tt.cursor
			}

			m.ensureLeftCursorVisible()

			if m.leftScroll != tt.wantLeftScroll {
				t.Fatalf("leftScroll = %d, want %d", m.leftScroll, tt.wantLeftScroll)
			}
			// The clamp must never exceed the maximum the renderer would allow.
			perItem := 3
			count := tt.sessions
			if tt.section == 2 {
				perItem, count = 4, tt.memories
			}
			if count > 0 {
				maxScroll := max(2+count*perItem-max(tt.height-6, 1), 0)
				if m.leftScroll > maxScroll {
					t.Fatalf("leftScroll = %d exceeds maxScroll %d", m.leftScroll, maxScroll)
				}
				assertLeftCursorVisible(t, m)
			}
		})
	}
}

// TestSessionsKeyboardScrollFollowsCursor exercises the real handler wiring:
// holding ↓ past the last visible row must scroll the viewport, and ↑ must
// bring it back. Guards the reported regression (cursor moved but viewport
// stayed put). XDG_DATA_HOME is isolated so refreshStats touches a throwaway DB.
func TestSessionsKeyboardScrollFollowsCursor(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	m := Model{
		currentSection: 1,
		focusPanel:     focusSessions,
		height:         14, // bodyHeight = 8 → ~2 sessions fit
		sessions:       mkSessions(20),
	}
	defer func() {
		if m.globalDB != nil {
			m.globalDB.Close()
		}
	}()

	scrolled := false
	for i := 1; i < len(m.sessions); i++ {
		m = m.mainKeyDown()
		if m.cursor != i {
			t.Fatalf("after %d downs, cursor = %d, want %d", i, m.cursor, i)
		}
		assertLeftCursorVisible(t, m)
		if m.leftScroll > 0 {
			scrolled = true
		}
	}
	if !scrolled {
		t.Fatal("viewport never scrolled while paging down — leftScroll stayed at 0")
	}

	for i := len(m.sessions) - 2; i >= 0; i-- {
		m = m.mainKeyUp()
		if m.cursor != i {
			t.Fatalf("cursor = %d, want %d", m.cursor, i)
		}
		assertLeftCursorVisible(t, m)
	}
	if m.leftScroll != 0 {
		t.Fatalf("back at the first session, leftScroll = %d, want 0 (header revealed)", m.leftScroll)
	}

	// PageDown/PageUp must keep the selection visible too.
	m = m.mainKeyPageDown()
	assertLeftCursorVisible(t, m)
	m = m.mainKeyPageUp()
	assertLeftCursorVisible(t, m)
}

// TestMemoryKeyboardScrollFollowsCursor proves the same wiring for the Memory
// section (section 2), which shares leftScroll but takes no DB path.
func TestMemoryKeyboardScrollFollowsCursor(t *testing.T) {
	m := Model{
		currentSection: 2,
		focusPanel:     focusSessions,
		height:         12, // bodyHeight = 6
		memories:       mkMemories(15),
	}

	scrolled := false
	for i := 1; i < len(m.memories); i++ {
		m = m.mainKeyDown()
		if m.memoryCursor != i {
			t.Fatalf("memoryCursor = %d, want %d", m.memoryCursor, i)
		}
		assertLeftCursorVisible(t, m)
		if m.leftScroll > 0 {
			scrolled = true
		}
	}
	if !scrolled {
		t.Fatal("memory viewport never scrolled while paging down")
	}

	for i := len(m.memories) - 2; i >= 0; i-- {
		m = m.mainKeyUp()
		assertLeftCursorVisible(t, m)
	}
	if m.leftScroll != 0 {
		t.Fatalf("back at the first memory, leftScroll = %d, want 0", m.leftScroll)
	}
}
