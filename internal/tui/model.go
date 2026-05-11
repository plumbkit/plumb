package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/stats"
)

// Version is set by the cli package before calling Run so it appears in the header.
var Version string

const (
	defaultLeftWidth = 26
	minLeftWidth     = 16
	minPopupLeftWidth = 30 // enough for " > ● ✓ 05-12 00:00:00 000ms"
	pollInterval     = 2 * time.Second
)

// pollMsg is sent by the periodic refresh tick.
type pollMsg struct{}

// panelFocus identifies which panel / section consumes navigation keys.
type panelFocus int

const (
	focusSessions  panelFocus = iota // j/k moves the session cursor (default)
	focusToolStats                   // j/k moves the Tool Statistics cursor
	focusStats                       // j/k moves the Recent calls cursor
)

// Model is the root Bubble Tea model for the sessions dashboard.
// Concurrency: single goroutine (Bubble Tea runtime).
type Model struct {
	sessions        []session.Info
	statsDBs        map[string]*stats.DB // cached per-workspace DBs (read-only)
	toolStats       []stats.ToolStat     // stats for currently selected session (all tools)
	recentCalls     []stats.RecentCall
	cursor          int        // index into m.sessions
	statsCursor     int        // index into m.recentCalls when focusPanel == focusStats
	toolStatsCursor int        // index into m.toolStats when focusPanel == focusToolStats
	focusPanel      panelFocus // which section j/k controls
	leftWidth       int        // width of left panel content column (excludes border chars)
	width           int
	height          int
	ready           bool
	loadErr         string

	// Popup overlay — per-tool call history view.
	// Triggered by enter on Tool Statistics or Recent rows.
	showPopup            bool               // popup is active
	popupTool            string             // tool whose history is shown
	popupCalls           []stats.RecentCall // workspace-wide calls for popupTool
	popupCallCursor      int                // cursor in the popup call list (left col)
	popupRightFocus      bool               // true = right (detail) col is focused for scrolling
	popupDetailScroll    int                // scroll offset into the detail column
	popupLeftWidth       int                // width of the popup left (timestamp) column

	// Table layout hints populated by rightLines() so mouse clicks can be
	// mapped to the correct row index without re-running the full render.
	statsTableBodyRow  int // 0-based body-row index of first stats data row (-1 if absent)
	recentTableBodyRow int // 0-based body-row index of first recent data row (-1 if absent)
}

// NewModel returns the initial model, loading sessions immediately.
func NewModel() Model {
	m := Model{leftWidth: defaultLeftWidth, statsDBs: make(map[string]*stats.DB)}
	m.refresh()
	return m
}

func (m *Model) refresh() {
	all, err := session.List()
	if err != nil {
		m.loadErr = err.Error()
		return
	}
	m.loadErr = ""

	// Show all active sessions — even those still resolving their workspace
	// (Folder == ""). Pending sessions appear with a ⟳ indicator in the left
	// panel so the user can see the connection is alive immediately on startup.
	m.sessions = all

	if m.cursor >= len(m.sessions) && m.cursor > 0 {
		m.cursor = len(m.sessions) - 1
	}
	m.refreshStats()
}

// dbFor returns the read-only stats DB for a workspace, opening it lazily.
// Returns nil if no DB exists yet (no calls recorded for that workspace).
//
// We only cache non-nil handles. If the DB file doesn't exist yet — for
// example, the TUI was opened before the first tool call created
// <workspace>/.plumb/stats.db — caching nil would freeze the panel at
// "No calls recorded yet" forever, because the daemon creates the file
// after we've already stored a nil. Re-attempting Open each poll while
// nil is cheap (one os.Stat) and lets the TUI pick up writes once they
// start.
func (m *Model) dbFor(workspace string) *stats.DB {
	if workspace == "" {
		return nil
	}
	if db, ok := m.statsDBs[workspace]; ok && db != nil {
		return db
	}
	db, _ := stats.OpenReadOnly(stats.DBPathFor(workspace))
	if db != nil {
		m.statsDBs[workspace] = db
	}
	return db
}

func (m *Model) refreshStats() {
	if len(m.sessions) == 0 {
		m.toolStats = nil
		m.recentCalls = nil
		return
	}
	s := m.sessions[m.cursor]
	db := m.dbFor(s.Folder)
	if db == nil {
		m.toolStats = nil
		m.recentCalls = nil
		return
	}
	// Capture identity of the currently selected call BEFORE the refresh so
	// we can re-locate it afterwards. db.Recent returns rows ordered DESC by
	// called_at, so newly-arrived calls prepend and naive index-based
	// cursors silently point at a different call — the Call Detail panel
	// then appears to "jump" under the user. (SessionID, CalledAtUnixMilli)
	// is unique enough: per-session calls are serialised in OnAfterTool.
	var prevTool string
	if m.toolStatsCursor < len(m.toolStats) {
		prevTool = m.toolStats[m.toolStatsCursor].Tool
	}
	prevCall := selectedCallKey(m.recentCalls, m.statsCursor)

	filter := stats.Filter{SessionID: s.ID}
	m.toolStats, _ = db.Summary(filter)
	m.recentCalls, _ = db.Recent(50, filter)

	m.statsCursor = locateCall(m.recentCalls, prevCall, m.statsCursor)
	m.toolStatsCursor = locateTool(m.toolStats, prevTool, m.toolStatsCursor)
}

// callKey identifies a recorded call across refreshes. Stable as long as the
// row survives the 50-row Recent() limit.
type callKey struct {
	sessionID  string
	calledAtMs int64
}

func (k callKey) zero() bool { return k.sessionID == "" && k.calledAtMs == 0 }

func selectedCallKey(calls []stats.RecentCall, idx int) callKey {
	if idx < 0 || idx >= len(calls) {
		return callKey{}
	}
	return callKey{sessionID: calls[idx].SessionID, calledAtMs: calls[idx].CalledAt.UnixMilli()}
}

// locateCall returns the new cursor position after a refresh: the index of
// the previously-selected call if it's still in the list, else a clamped
// fallback (the previous index, capped at list length).
func locateCall(calls []stats.RecentCall, key callKey, fallback int) int {
	if !key.zero() {
		for i, c := range calls {
			if c.SessionID == key.sessionID && c.CalledAt.UnixMilli() == key.calledAtMs {
				return i
			}
		}
	}
	if fallback >= len(calls) {
		if len(calls) == 0 {
			return 0
		}
		return len(calls) - 1
	}
	if fallback < 0 {
		return 0
	}
	return fallback
}

// locateTool returns the index of toolName in stats, else a clamped fallback.
func locateTool(stats []stats.ToolStat, toolName string, fallback int) int {
	if toolName != "" {
		for i, t := range stats {
			if t.Tool == toolName {
				return i
			}
		}
	}
	if fallback >= len(stats) {
		if len(stats) == 0 {
			return 0
		}
		return len(stats) - 1
	}
	if fallback < 0 {
		return 0
	}
	return fallback
}

// refreshPopupCalls loads workspace-wide call history for popupTool.
// Like refreshStats, preserves the selected call by (session_id, called_at)
// so newly-recorded calls prepending to the list don't shift the user's
// selection to a different call.
func (m *Model) refreshPopupCalls() {
	if m.popupTool == "" || len(m.sessions) == 0 {
		m.popupCalls = nil
		return
	}
	ws := m.sessions[m.cursor].Folder
	db := m.dbFor(ws)
	if db == nil {
		m.popupCalls = nil
		return
	}
	prev := selectedCallKey(m.popupCalls, m.popupCallCursor)
	m.popupCalls, _ = db.CallsForTool(m.popupTool, ws, 200)
	m.popupCallCursor = locateCall(m.popupCalls, prev, m.popupCallCursor)
}

// openPopup opens the call history popup for the given tool and optionally
// pre-positions the cursor to the call with the given timestamp.
func (m *Model) openPopup(tool string, preselect time.Time) {
	m.showPopup = true
	m.popupTool = tool
	m.popupCallCursor = 0
	m.popupRightFocus = false
	m.popupDetailScroll = 0
	if m.popupLeftWidth == 0 {
		m.popupLeftWidth = minPopupLeftWidth
	}
	// Pre-position cursor if a specific call was requested.
	if !preselect.IsZero() {
		for i, c := range m.popupCalls {
			if c.CalledAt.Equal(preselect) {
				m.popupCallCursor = i
				break
			}
		}
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pollMsg:
		m.refresh()
		if m.showPopup {
			m.refreshPopupCalls()
		}
		return m, tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true

	case tea.MouseClickMsg:
		// bodyTop: rows above the body content.
		// Normal:  header(1) + top-border(1) = 2
		// Popup:   header(1) + main-border(1) + popup-border(1) = 3
		bodyTop := 2
		if m.showPopup {
			bodyTop = 3
		}
		bodyRow := msg.Y - bodyTop
		bodyHeight := m.height - 4
		if m.showPopup {
			bodyHeight = m.height - 5
		}

		if m.showPopup {
			// Left panel: click on a timestamp row selects that call.
			if msg.X >= 1 && msg.X <= m.leftWidth {
				// popupLeftLines starts with one blank line at index 0;
				// call rows start at index 1.
				callIdx := bodyRow - 1
				if callIdx >= 0 && callIdx < len(m.popupCalls) {
					m.popupCallCursor = callIdx
					m.popupRightFocus = false
					m.popupDetailScroll = 0
				}
			}
			// Right panel: click anywhere switches focus to detail panel.
			if msg.X > m.leftWidth+2 {
				m.popupRightFocus = true
			}
			break
		}

		// Normal mode — left panel: click on a session row.
		if bodyRow >= 0 && bodyRow < bodyHeight {
			if msg.X >= 1 && msg.X <= m.leftWidth {
				sessionIdx := bodyRow - 1 // first line is blank
				if sessionIdx >= 0 && sessionIdx < len(m.sessions) {
					m.cursor = sessionIdx
					m.focusPanel = focusSessions
					m.refreshStats()
				}
			}
		}

		// Normal mode — right panel: click in Statistics or Recent tables.
		if msg.X > m.leftWidth+2 {
			// Ensure table body row offsets are current before mapping the click.
			rightWidth := m.width - m.leftWidth - 3
			if rightWidth < 10 {
				rightWidth = 10
			}
			(&m).rightLines(rightWidth) // populates statsTableBodyRow / recentTableBodyRow
			(&m).handleRightPanelClick(bodyRow)
		}

	case tea.MouseWheelMsg:
		if m.showPopup {
			// Wheel scrolls whichever panel is active.
			if m.popupRightFocus {
				if msg.Button == tea.MouseWheelDown {
					m.popupDetailScroll++
				} else if msg.Button == tea.MouseWheelUp && m.popupDetailScroll > 0 {
					m.popupDetailScroll--
				}
			} else {
				if msg.Button == tea.MouseWheelDown {
					if m.popupCallCursor < len(m.popupCalls)-1 {
						m.popupCallCursor++
						m.popupDetailScroll = 0
					}
				} else if msg.Button == tea.MouseWheelUp {
					if m.popupCallCursor > 0 {
						m.popupCallCursor--
						m.popupDetailScroll = 0
					}
				}
			}
			break
		}
		if msg.Button == tea.MouseWheelDown {
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
			}
		} else if msg.Button == tea.MouseWheelUp {
			if m.cursor > 0 {
				m.cursor--
			}
		}

	case tea.KeyPressMsg:
		// ── Popup keys (take full priority when popup is open) ────────────
		if m.showPopup {
			switch msg.String() {
			case "q", "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.showPopup = false
				m.popupCalls = nil
				m.popupDetailScroll = 0
				m.popupRightFocus = false
			case "tab":
				m.popupRightFocus = !m.popupRightFocus
				m.popupDetailScroll = 0
			case "up", "k":
				if m.popupRightFocus {
					if m.popupDetailScroll > 0 {
						m.popupDetailScroll--
					}
				} else {
					if m.popupCallCursor > 0 {
						m.popupCallCursor--
						m.popupDetailScroll = 0
					}
				}
			case "down", "j":
				if m.popupRightFocus {
					m.popupDetailScroll++
				} else {
					if m.popupCallCursor < len(m.popupCalls)-1 {
						m.popupCallCursor++
						m.popupDetailScroll = 0
					}
				}
			case "c":
				// Copy args+output to clipboard via pbcopy/xclip.
				if len(m.popupCalls) > 0 {
					return m, copyToClipboard(m.popupCalls[m.popupCallCursor])
				}
			case "[":
				m.popupLeftWidth -= 2
				if m.popupLeftWidth < minPopupLeftWidth {
					m.popupLeftWidth = minPopupLeftWidth
				}
			case "]":
				m.popupLeftWidth += 2
				maxPLeft := m.width/2
				if m.popupLeftWidth > maxPLeft {
					m.popupLeftWidth = maxPLeft
				}
			}
			break
		}

		// ── Normal keys ───────────────────────────────────────────────────
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			// nothing to dismiss in normal mode
		case "enter":
			switch m.focusPanel {
			case focusToolStats:
				if len(m.toolStats) > 0 {
					m.openPopup(m.toolStats[m.toolStatsCursor].Tool, time.Time{})
				}
			case focusStats:
				if len(m.recentCalls) > 0 {
					rc := m.recentCalls[m.statsCursor]
					m.openPopup(rc.Tool, rc.CalledAt)
				}
			}
		case "tab":
			// Cycle forward: Sessions → ToolStats → Recent → Sessions
			switch m.focusPanel {
			case focusSessions:
				if len(m.toolStats) > 0 {
					m.focusPanel = focusToolStats
				} else if len(m.recentCalls) > 0 {
					m.focusPanel = focusStats
				}
			case focusToolStats:
				if len(m.recentCalls) > 0 {
					m.focusPanel = focusStats
				} else {
					m.focusPanel = focusSessions
				}
			case focusStats:
				m.focusPanel = focusSessions
			}
		case "shift+tab":
			// Cycle backward: Sessions → Recent → ToolStats → Sessions
			switch m.focusPanel {
			case focusSessions:
				if len(m.recentCalls) > 0 {
					m.focusPanel = focusStats
				} else if len(m.toolStats) > 0 {
					m.focusPanel = focusToolStats
				}
			case focusStats:
				if len(m.toolStats) > 0 {
					m.focusPanel = focusToolStats
				} else {
					m.focusPanel = focusSessions
				}
			case focusToolStats:
				m.focusPanel = focusSessions
			}
		case "up", "k":
			switch m.focusPanel {
			case focusToolStats:
				if m.toolStatsCursor > 0 {
					m.toolStatsCursor--
				}
			case focusStats:
				if m.statsCursor > 0 {
					m.statsCursor--
				}
			default:
				if m.cursor > 0 {
					m.cursor--
					m.refreshStats()
				}
			}
		case "down", "j":
			switch m.focusPanel {
			case focusToolStats:
				if m.toolStatsCursor < len(m.toolStats)-1 {
					m.toolStatsCursor++
				}
			case focusStats:
				if m.statsCursor < len(m.recentCalls)-1 {
					m.statsCursor++
				}
			default:
				if m.cursor < len(m.sessions)-1 {
					m.cursor++
					m.refreshStats()
				}
			}
		case "a":
			m.refresh()
		case "[":
			m.leftWidth -= 2
			if m.leftWidth < minLeftWidth {
				m.leftWidth = minLeftWidth
			}
		case "]":
			m.leftWidth += 2
			maxLeft := m.width - 23
			if maxLeft < minLeftWidth {
				maxLeft = minLeftWidth
			}
			if m.leftWidth > maxLeft {
				m.leftWidth = maxLeft
			}
		}
	}
	return m, nil
}

func (m Model) View() tea.View {
	content := "Loading…"
	if m.ready {
		content = m.render()
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m Model) render() string {
	rightWidth := m.width - m.leftWidth - 3
	if rightWidth < 10 {
		rightWidth = 10
	}
	bodyHeight := m.height - 4 // header + frame-top + frame-bottom + footer
	if m.showPopup {
		// Popup steals one extra row for its own top border, so body is one shorter.
		bodyHeight = m.height - 5
	}
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	var sb strings.Builder

	// Header line
	titleText := "plumb"
	if Version != "" {
		titleText += " " + Version
	}
	title := TitleStyle.Render(titleText)
	var hint string
	if m.showPopup {
		hint = HintStyle.Render("↑↓/jk calls · tab scroll detail · esc close · q quit")
	} else {
		hint = HintStyle.Render("↑↓/jk navigate · tab/shift+tab focus · enter popup · [/] resize · q quit")
	}
	gap := m.width - lipgloss.Width(title) - lipgloss.Width(hint)
	if gap < 1 {
		gap = 1
	}
	sb.WriteString(title + strings.Repeat(" ", gap) + hint + "\n")

	// Main border — always on row 2.
	if m.showPopup {
		// Popup widths — compute here so both border rows use the same values.
		if m.popupLeftWidth == 0 {
			m.popupLeftWidth = minPopupLeftWidth
		}
		pLW := m.popupLeftWidth
		pRW := m.width - pLW - 3
		if pRW < 10 {
			pRW = 10
		}
		// Dim background border uses main-panel leftWidth & rightWidth.
		sb.WriteString(m.renderTopBorderDim(rightWidth) + "\n")
		// Popup border uses popup widths.
		sb.WriteString(m.renderTopBorderPopup(pLW, pRW) + "\n")
	} else {
		sb.WriteString(m.renderTopBorder(rightWidth) + "\n")
	}

	var leftLines, rightLines []string
	var scrollbar []string

 	if m.showPopup {
		// In popup mode, use popupLeftWidth for the timestamp panel.
		if m.popupLeftWidth == 0 {
			m.popupLeftWidth = minPopupLeftWidth
		}
		pLW := m.popupLeftWidth
		pRW := m.width - pLW - 3 // left-│ + ┆ + right-│
		if pRW < 10 {
			pRW = 10
		}

		bgLeft := m.dimLines(m.leftLines(), m.leftWidth)

		popupLeft := m.popupLeftLines()
		allRight := m.popupRightAll(pRW - 2) // -2: 1 for scrollbar col, 1 for gap
		total := len(allRight)
		start := m.popupDetailScroll
		if start > total {
			start = total
		}
		popupRight := allRight[start:]
		scrollbar = scrollbarCol(total, bodyHeight, start)

		for i := range bodyHeight {
			var lCell string
			if i < len(popupLeft) && popupLeft[i] != "" {
				lCell = lipgloss.NewStyle().Width(pLW).Render(popupLeft[i])
			} else if i < len(bgLeft) {
				lCell = lipgloss.NewStyle().Width(pLW).Render(bgLeft[i])
			} else {
				lCell = lipgloss.NewStyle().Width(pLW).Render("")
			}

			var rStr string
			if i < len(popupRight) {
				rStr = popupRight[i]
			}
			rCell := lipgloss.NewStyle().Width(pRW).Render(rStr)

			var rightBorder string
			if scrollbar != nil && i < len(scrollbar) {
				rightBorder = scrollbar[i]
			} else {
				rightBorder = SepStyle.Render("│")
			}
			sb.WriteString(SepStyle.Render("│") + lCell + SepStyle.Render("┆") + rCell + rightBorder + "\n")
		}
	} else {
		leftLines = m.leftLines()
		rightLines = (&m).rightLines(rightWidth)

		for i := range bodyHeight {
			l, r := "", ""
			if i < len(leftLines) {
				l = leftLines[i]
			}
			if i < len(rightLines) {
				r = rightLines[i]
			}
			leftCell := lipgloss.NewStyle().Width(m.leftWidth).Render(l)
			rightCell := lipgloss.NewStyle().Width(rightWidth).Render(r)
			sb.WriteString(SepStyle.Render("│") + leftCell + SepStyle.Render("┆") + rightCell + SepStyle.Render("│") + "\n")
		}
	}

	if m.showPopup {
		sb.WriteString(m.renderBottomBorderPopup(m.popupLeftWidth, m.width-m.popupLeftWidth-3) + "\n")
	} else {
		sb.WriteString(m.renderBottomBorder(rightWidth) + "\n")
	}

	// Footer — sum across the visible sessions' per-project DBs.
	var totalCalls, savedTok int64
	for _, s := range m.sessions {
		db := m.dbFor(s.Folder)
		if db == nil {
			continue
		}
		totalCalls += db.TotalCalls(stats.Filter{})
		savedTok += db.TotalTokensSaved(stats.Filter{})
	}
	footer := fmt.Sprintf("%d session(s)  ·  %d tool calls  ·  ~%s tokens saved",
		len(m.sessions), totalCalls, stats.FormatSavings(int(savedTok)))

	if m.loadErr != "" {
		footer += "  ·  " + m.loadErr
	}
	sb.WriteString(StatusStyle.Render(footer))

	return sb.String()
}

// renderTopBorder builds the top frame line.
// In popup mode the left/right titles dim based on which panel is focused.
func (m Model) renderTopBorder(rightWidth int) string {
	var leftTitle, rightTitle string
	var leftTitleStyle, rightTitleStyle lipgloss.Style
	if m.showPopup {
		leftTitle = " Timestamp "
		rightTitle = " Call Detail "
		// Faded whichever panel is not focused.
		if m.popupRightFocus {
			leftTitleStyle = PanelHeaderFadedStyle  // Timestamp faded: #6C768A
			rightTitleStyle = PanelHeaderStyle
		} else {
			leftTitleStyle = PanelHeaderStyle
			rightTitleStyle = PanelHeaderFadedStyle // Call Detail faded: #6C768A
		}
	} else {
		leftTitle = fmt.Sprintf(" Sessions (%d) ", len(m.sessions))
		rightTitle = " Session + Stats "
		// Faded the unfocused panel title in normal mode.
		if m.focusPanel == focusSessions {
			leftTitleStyle = PanelHeaderStyle
			rightTitleStyle = PanelHeaderFadedStyle // Session + Stats faded: #6C768A
		} else {
			leftTitleStyle = PanelHeaderFadedStyle   // Sessions faded: #6C768A
			rightTitleStyle = PanelHeaderStyle
		}
	}

	// Use popupLeftWidth for the left panel width in popup mode.
	actualLeftW := m.leftWidth
	if m.showPopup && m.popupLeftWidth > 0 {
		actualLeftW = m.popupLeftWidth
	}

	leftTitleVis := len(leftTitle)
	leftFill := actualLeftW - 1 - leftTitleVis
	if leftFill < 0 {
		avail := actualLeftW - 2
		if avail > 0 {
			leftTitle = leftTitle[:avail] + " "
		} else {
			leftTitle = ""
		}
		leftTitleVis = len(leftTitle)
		leftFill = actualLeftW - 1 - leftTitleVis
		if leftFill < 0 {
			leftFill = 0
		}
	}

	rightTitleVis := len(rightTitle)
	rightFill := rightWidth - 1 - rightTitleVis
	if rightFill < 0 {
		rightTitle = ""
		rightFill = rightWidth - 1
		if rightFill < 0 {
			rightFill = 0
		}
	}

	return SepStyle.Render("╭─") +
		leftTitleStyle.Render(leftTitle) +
		SepStyle.Render(strings.Repeat("─", leftFill)+"┬─") +
		rightTitleStyle.Render(rightTitle) +
		SepStyle.Render(strings.Repeat("─", rightFill)+"╮")
}

// renderTopBorderDim renders the main panel top border in DimStyle.
// Used in popup mode to show the main border faded behind the popup border.
func (m Model) renderTopBorderDim(rightWidth int) string {
	leftTitle := fmt.Sprintf(" Sessions (%d) ", len(m.sessions))
	rightTitle := " Session + Stats "

	leftTitleVis := len(leftTitle)
	leftFill := m.leftWidth - 1 - leftTitleVis
	if leftFill < 0 {
		leftFill = 0
	}
	rightFill := rightWidth - 1 - len(rightTitle)
	if rightFill < 0 {
		rightFill = 0
	}
	return SepInactiveStyle.Render("╭─") +
		SepInactiveStyle.Render(leftTitle) +
		SepInactiveStyle.Render(strings.Repeat("─", leftFill)+"┬─") +
		SepInactiveStyle.Render(rightTitle) +
		SepInactiveStyle.Render(strings.Repeat("─", rightFill)+"╮")
}

func (m Model) renderBottomBorder(rightWidth int) string {
	return SepStyle.Render(
		"╰" + strings.Repeat("─", m.leftWidth) + "┴" + strings.Repeat("─", rightWidth) + "╯",
	)
}

// renderTopBorderPopup builds the popup's own top border using popup-specific widths.
func (m Model) renderTopBorderPopup(pLW, pRW int) string {
	leftTitle := " Timestamp "
	rightTitle := " Call Detail "
	var leftTitleStyle, rightTitleStyle lipgloss.Style
	if m.popupRightFocus {
		leftTitleStyle = PanelHeaderFadedStyle
		rightTitleStyle = PanelHeaderStyle
	} else {
		leftTitleStyle = PanelHeaderStyle
		rightTitleStyle = PanelHeaderFadedStyle
	}
	leftFill := pLW - 1 - len(leftTitle)
	if leftFill < 0 {
		leftFill = 0
	}
	rightFill := pRW - 1 - len(rightTitle)
	if rightFill < 0 {
		rightFill = 0
	}
	return SepStyle.Render("╭─") +
		leftTitleStyle.Render(leftTitle) +
		SepStyle.Render(strings.Repeat("─", leftFill)+"┬─") +
		rightTitleStyle.Render(rightTitle) +
		SepStyle.Render(strings.Repeat("─", rightFill)+"╮")
}

// renderBottomBorderPopup builds the bottom border using popup-specific widths.
func (m Model) renderBottomBorderPopup(pLW, pRW int) string {
	return SepStyle.Render(
		"╰" + strings.Repeat("─", pLW) + "┴" + strings.Repeat("─", pRW) + "╯",
	)
}

// dimLines takes a slice of pre-rendered lines (which may contain ANSI escape
// codes) and re-renders each at the given width in DimStyle, fading the entire
// panel for the popup overlay effect.
func (m Model) dimLines(lines []string, width int) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		// lipgloss.Width strips ANSI; we measure, then pad to the target width.
		vis := lipgloss.Width(l)
		plain := lipgloss.NewStyle().Width(width).Render(l)
		_ = vis
		out[i] = InactiveStyle.Render(plain)
	}
	return out
}

// popupLeftLines renders the call list column for the popup overlay.
func (m Model) popupLeftLines() []string {
	lines := []string{""}

	if len(m.popupCalls) == 0 {
		lines = append(lines, "   "+MutedStyle.Render("No calls recorded yet."))
		return lines
	}

	var currentSessID string
	if len(m.sessions) > 0 {
		currentSessID = m.sessions[m.cursor].ID
	}

	for i, c := range m.popupCalls {
		selected := !m.popupRightFocus && i == m.popupCallCursor
		isCursor := i == m.popupCallCursor

		// 1-char gap on each side: " > " for cursor, "   " for others.
		prefix := "   "
		if isCursor {
			prefix = " > "
		}

		sessChar := "○"
		if c.SessionID == currentSessID {
			sessChar = "●"
		}
		okChar := "✓"
		if !c.Success {
			okChar = "✗"
		}
		ts := c.CalledAt.Format("01-02 15:04:05")
		dur := fmt.Sprintf("%dms", c.DurationMs)
		row := fmt.Sprintf("%s%s %s %s %s", prefix, sessChar, okChar, ts, dur)
		// Truncate to panel width so long durations never wrap.
		maxW := m.popupLeftWidth - 1
		if maxW < 10 {
			maxW = 10
		}
		if lipgloss.Width(row) > maxW {
			row = string([]rune(row)[:maxW-1]) + "…"
		}

		if selected {
			lines = append(lines, SelectedStyle.Render(row))
		} else if m.popupRightFocus {
			// Left panel is in background — all rows use the disabled colour.
			lines = append(lines, TimestampFadedStyle.Render(row))
		} else {
			lines = append(lines, TimestampActiveStyle.Render(row))
		}
	}
	return lines
}

// popupRightAll returns all call-detail lines for the popup right panel,
// without applying any scroll offset. The caller windows and scrolls them.
// rightWidth should already be reduced by 2 to leave room for gap + scrollbar.
func (m Model) popupRightAll(rightWidth int) []string {
	lines := []string{""}

	if len(m.popupCalls) == 0 {
		lines = append(lines, "  "+MutedStyle.Render("No calls to show."))
		return lines
	}

	c := m.popupCalls[m.popupCallCursor]

	var currentSessID string
	if len(m.sessions) > 0 {
		currentSessID = m.sessions[m.cursor].ID
	}

	status := OkStyle.Render("✓ success")
	if !c.Success {
		status = WarnStyle.Render("✗ failed")
	}

	sessLabel := MutedStyle.Render("○ historical")
	if c.SessionID == currentSessID {
		sessLabel = OkStyle.Render("● current")
	}
	shortSessID := c.SessionID
	if len(shortSessID) > 12 {
		shortSessID = shortSessID[:12] + "…"
	}

	// Call Detail section — no redundant header; the panel border already says
	// "Call Detail". Start directly with the key-value rows.
	lines = append(lines,
		detailRow("Tool", DetailStyle.Render(c.Tool)),
		detailRow("Status", status),
		detailRow("Called at", DetailStyle.Render(c.CalledAt.Format("2006-01-02 15:04:05"))),
		detailRow("Session", shortSessID+"  "+sessLabel),
		detailRow("Duration", DetailStyle.Render(fmt.Sprintf("%d ms", c.DurationMs))),
		detailRow("Input", DetailStyle.Render(fmt.Sprintf("%d bytes", c.InputBytes))),
		detailRow("Output", DetailStyle.Render(fmt.Sprintf("%d bytes", c.OutputBytes))),
	)

	// boxSection renders a named rounded box.
	// Layout: " " + "│" + " " + padded(inner) + " " + "│" = inner+4 visible chars + 1 left margin = inner+5.
	// The cell width available is rightWidth (already pRW-2, accounting for scrollbar+gap).
	// We want box right "│" to sit at rightWidth-1, leaving 1 space before panel border:
	// 1 + 1 + 1 + inner + 1 + 1 = inner+5 = rightWidth, so inner = rightWidth - 5.
	// Bottom/top horizontal span: "╭─" + topLabel + fill + "╮" must equal inner+4:
	//   topFill = inner + 2 - len(topLabel)
	// Bottom dashes: inner + 2.
	boxSection := func(label string, contentLines []string) {
		inner := rightWidth - 5
		if inner < 8 {
			inner = 8
		}
		topLabel := " " + label + " "
		topFill := inner + 2 - len(topLabel)
		if topFill < 0 {
			topFill = 0
		}
		topBorder := " " + SepStyle.Render("╭─") + PanelHeaderStyle.Render(topLabel) + SepStyle.Render(strings.Repeat("─", topFill)+"╮")
		lines = append(lines, "", topBorder)
		for _, cl := range contentLines {
			if lipgloss.Width(cl) > inner {
				cl = string([]rune(cl)[:inner-1]) + "…"
			}
			padded := lipgloss.NewStyle().Width(inner).Render(cl)
			lines = append(lines, " "+SepStyle.Render("│")+" "+padded+" "+SepStyle.Render("│"))
		}
		lines = append(lines, " "+SepStyle.Render("╰"+strings.Repeat("─", inner+2)+"╯"))
	}

	// Error section as a box.
	if !c.Success {
		var errLines []string
		if c.ErrorMsg != "" {
			for _, w := range wrapText(c.ErrorMsg, rightWidth-5) {
				errLines = append(errLines, WarnStyle.Render(w))
			}
		} else {
			errLines = append(errLines, MutedStyle.Render("(no error message recorded)"))
		}
		boxSection("Error", errLines)
	}

	if c.InputJSON != "" {
		var argsLines []string
		var prettyBuf bytes.Buffer
		if err := json.Indent(&prettyBuf, []byte(c.InputJSON), "", "  "); err == nil {
			for _, l := range strings.Split(strings.TrimRight(prettyBuf.String(), "\n"), "\n") {
				argsLines = append(argsLines, DetailStyle.Render(l))
			}
		} else {
			argsLines = append(argsLines, DetailStyle.Render(c.InputJSON))
		}
		boxSection("Args", argsLines)
	}

	if c.OutputText != "" && c.Success {
		var outLines []string
		for _, ol := range strings.Split(strings.TrimRight(c.OutputText, "\n"), "\n") {
			outLines = append(outLines, DetailStyle.Render(ol))
		}
		boxSection("Output", outLines)
	}

	if m.popupRightFocus {
		lines = append(lines, "", "  "+MutedStyle.Render("c copy · tab back"))
	}

	return lines
}

// scrollbarCol builds a slice of bodyHeight single-character strings
// representing a vertical scrollbar. Returns nil when content fits.
// Thumb symbol: ▐ (right half-block). Track symbol: │ (light vertical bar), faded.
func scrollbarCol(total, visible, offset int) []string {
	if total <= visible {
		return nil
	}

	thumbSize := visible * visible / total
	if thumbSize < 1 {
		thumbSize = 1
	}
	maxOffset := total - visible
	if maxOffset < 1 {
		maxOffset = 1
	}
	thumbStart := offset * (visible - thumbSize) / maxOffset

	col := make([]string, visible)
	for i := range visible {
		if i >= thumbStart && i < thumbStart+thumbSize {
			col[i] = ScrollThumbStyle.Render("█")
		} else {
			col[i] = ScrollTrackStyle.Render("│")
		}
	}
	return col
}

// leftLines returns content rows for the left panel in normal (non-popup) mode.
// Content is dimmed when focus is on the right panel.
func (m Model) leftLines() []string {
	// leftFocused is true when the sessions panel holds focus.
	leftFocused := m.focusPanel == focusSessions

	lines := []string{""}

	if len(m.sessions) == 0 {
		msg1 := " Daemon running."
		msg2 := " Call a tool to begin."
		if !daemonRunning() {
			msg1 = " No active sessions."
			msg2 = ""
		}
		if leftFocused {
			lines = append(lines, MutedStyle.Render(msg1))
			if msg2 != "" {
				lines = append(lines, MutedStyle.Render(msg2))
			}
		} else {
			lines = append(lines, InactiveStyle.Render(msg1))
			if msg2 != "" {
				lines = append(lines, InactiveStyle.Render(msg2))
			}
		}
		return lines
	}

	for i, s := range m.sessions {
		var label string
		if s.Folder == "" {
			label = "⟳ resolving…"
		} else {
			langPrefix := s.Language + ": "
			// -4 for the " ▸ " / "   " prefix (3 chars) + 1 border
			maxFolder := m.leftWidth - 4 - len([]rune(langPrefix))
			if maxFolder < 0 {
				maxFolder = 0
			}
			label = langPrefix + contractPath(s.Folder, maxFolder)
		}
		if i == m.cursor {
			if leftFocused {
				lines = append(lines, SelectedStyle.Render(" > "+label))
			} else {
				lines = append(lines, FadedStyle.Render(" > "+label))
			}
		} else {
			if leftFocused {
				lines = append(lines, ItemStyle.Render("   "+label))
			} else {
				lines = append(lines, FadedStyle.Render("   "+label))
			}
		}
	}
	return lines
}

// handleRightPanelClick maps a body-row index (0-based, relative to the top
// content row of the right panel) to a table cursor selection.
// It uses the pre-computed statsTableBodyRow / recentTableBodyRow offsets set
// by the most recent rightLines() call.
func (m *Model) handleRightPanelClick(bodyRow int) {
	if m.statsTableBodyRow >= 0 && len(m.toolStats) > 0 {
		idx := bodyRow - m.statsTableBodyRow
		if idx >= 0 && idx < len(m.toolStats) {
			m.toolStatsCursor = idx
			m.focusPanel = focusToolStats
			return
		}
	}
	if m.recentTableBodyRow >= 0 && len(m.recentCalls) > 0 {
		idx := bodyRow - m.recentTableBodyRow
		if idx >= 0 && idx < len(m.recentCalls) {
			m.statsCursor = idx
			m.focusPanel = focusStats
			return
		}
	}
}

// rightLines returns content rows for the right panel in normal (non-popup) mode.
// It also populates m.statsTableBodyRow and m.recentTableBodyRow so that
// handleRightPanelClick can map body-row indices to table rows.
func (m *Model) rightLines(rightWidth int) []string {
	lines := []string{""}

	if len(m.sessions) == 0 {
		lines = append(lines, "  "+MutedStyle.Render("Select a session to view details."))
		return lines
	}

	const keyColWidth = 14
	maxVal := rightWidth - keyColWidth
	if maxVal < 8 {
		maxVal = 8
	}

	s := m.sessions[m.cursor]

	// ─── Session detail ───────────────────────────────────────────────────
	folder := s.Folder
	if folder == "" {
		folder = MutedStyle.Render("(resolving workspace…)")
	} else {
		folder = contractPath(folder, maxVal)
	}
	lines = append(lines,
		detailRow("ID", s.ID),
		detailRow("Language", s.Language),
		detailRow("Folder", folder),
		detailRow("Adapter", s.Adapter),
		detailRow("PID", fmt.Sprintf("%d", s.PID)),
	)
	if s.DaemonVersion != "" {
		lines = append(lines, detailRow("Daemon", s.DaemonVersion))
	}
	lines = append(lines, detailRow("Started", s.StartedAt.Format("2006-01-02 15:04:05")))

	client := s.ClientName
	if s.ClientVersion != "" {
		client += " " + s.ClientVersion
	}
	if client == "" {
		client = MutedStyle.Render("unknown")
	}
	lines = append(lines, detailRow("Client", client))

	// Shared column layout for both Statistics and Recent tables.
	// All four columns are at identical pixel offsets in both tables so
	// they read as a single aligned grid.
	//
	//  col1 (Tool)   col2 (Calls/Dur)  col3 (Avg/When)  col4 (Errors)
	//  left-aligned  right-aligned     right-aligned    right-aligned
	//
	const (
		col2W = 8  // right-aligned: Calls / Dur
		col3W = 10 // right-aligned: Avg / When
		col4W = 6  // right-aligned: Errors count or ✗, with trailing gap before border
	)
	// 3-space gap between every column, plus 3 trailing spaces after col4.
	sep3 := "   "
	// Layout: 2 + col1 + 3 + col2 + 3 + col3 + 3 + col4 + 3 = rightWidth
	col1W := rightWidth - 2 - col2W - col3W - col4W - 12
	if col1W < 10 {
		col1W = 10
	}

	sepLine := "  " + SepStyle.Render(strings.Repeat("─", rightWidth-3))
	// rowWidth: full width for selected-row rendering.
	rowWidth := rightWidth - 2

	// ── Statistics table ──────────────────────────────────────────────────
	lines = append(lines, "")

	if len(m.toolStats) == 0 {
		// No data: just show the label with no header row.
		if m.focusPanel == focusToolStats {
			lines = append(lines, "  "+SelectedStyle.Render("Tools"))
		} else {
			lines = append(lines, "  "+HintStyle.Render("Tools"))
		}
		lines = append(lines, "  "+MutedStyle.Render("No calls recorded yet."))
		m.statsTableBodyRow = -1
	} else {
		// Section label IS the col-1 header cell — no separate label row.
		var labelCell string
		if m.focusPanel == focusToolStats {
			labelCell = padRight(SelectedStyle.Render("Tools"), col1W)
		} else {
			labelCell = padRight(HintStyle.Render("Tools"), col1W)
		}
		tsHeader := "  " +
			labelCell + sep3 +
			padLeft(HintStyle.Render("Calls"), col2W) + sep3 +
			padLeft(HintStyle.Render("Avg"), col3W) + sep3 +
			HintStyle.Render("Errors")
		lines = append(lines, tsHeader, sepLine)
		m.statsTableBodyRow = len(lines)

		for i, ts := range m.toolStats {
			selected := m.focusPanel == focusToolStats && i == m.toolStatsCursor
			plainTool := padRight(toolIcon(ts.Tool)+" "+truncate(ts.Tool, col1W-2), col1W)
			if selected {
				plainCalls := padLeft(fmt.Sprintf("%d", ts.Calls), col2W)
				plainAvg := padLeft(fmt.Sprintf("%.0fms", ts.AvgMs), col3W)
				plainErr := padLeft("", col4W)
				if ts.Errors > 0 {
					plainErr = padLeft(fmt.Sprintf("%d", ts.Errors), col4W)
				}
				row := plainTool + sep3 + plainCalls + sep3 + plainAvg + sep3 + plainErr + sep3
				lines = append(lines, SelectedStyle.Width(rowWidth).Render("> "+row))
			} else {
				col2Cell := padLeft(OkStyle.Render(fmt.Sprintf("%d", ts.Calls)), col2W)
				col3Cell := padLeft(MutedStyle.Render(fmt.Sprintf("%.0fms", ts.AvgMs)), col3W)
				col4Cell := padLeft("", col4W)
				if ts.Errors > 0 {
					col4Cell = padLeft(WarnStyle.Render(fmt.Sprintf("%d", ts.Errors)), col4W)
				}
				row := plainTool + sep3 + col2Cell + sep3 + col3Cell + sep3 + col4Cell + sep3
				lines = append(lines, "  "+row)
			}
		}
	}

	// ─── Recent calls table ───────────────────────────────────────────────
	if len(m.recentCalls) > 0 {
		lines = append(lines, "")

		var rcLabelCell string
		if m.focusPanel == focusStats {
			rcLabelCell = padRight(SelectedStyle.Render("Recent Tools"), col1W)
		} else {
			rcLabelCell = padRight(HintStyle.Render("Recent Tools"), col1W)
		}
		rcHeader := "  " +
			rcLabelCell + sep3 +
			padLeft(HintStyle.Render("Dur"), col2W) + sep3 +
			padLeft(HintStyle.Render("When"), col3W) + sep3 +
			HintStyle.Render("Errors")
		lines = append(lines, rcHeader, sepLine)
		m.recentTableBodyRow = len(lines)

		for i, c := range m.recentCalls {
			selected := m.focusPanel == focusStats && i == m.statsCursor
			statusChar := "✓"
			if !c.Success {
				statusChar = "✗"
			}
			plainTool := padRight(statusChar+" "+toolIcon(c.Tool)+" "+truncate(c.Tool, col1W-4), col1W)
			if selected {
				plainDur := padLeft(fmt.Sprintf("%dms", c.DurationMs), col2W)
				plainWhen := padLeft(humanAgeTUI(c.CalledAt), col3W)
				plainErr := padLeft("", col4W)
				if !c.Success {
					plainErr = padLeft("✗", col4W)
				}
				row := plainTool + sep3 + plainDur + sep3 + plainWhen + sep3 + plainErr + sep3
				lines = append(lines, SelectedStyle.Width(rowWidth).Render("> "+row))
			} else {
				status := OkStyle.Render("✓")
				if !c.Success {
					status = WarnStyle.Render("✗")
				}
				toolCell := padRight(status+" "+toolIcon(c.Tool)+" "+truncate(c.Tool, col1W-4), col1W)
				col2Cell := padLeft(MutedStyle.Render(fmt.Sprintf("%dms", c.DurationMs)), col2W)
				col3Cell := padLeft(MutedStyle.Render(humanAgeTUI(c.CalledAt)), col3W)
				col4Cell := padLeft("", col4W)
				if !c.Success {
					col4Cell = padLeft(WarnStyle.Render("✗"), col4W)
				}
				row := toolCell + sep3 + col2Cell + sep3 + col3Cell + sep3 + col4Cell + sep3
				lines = append(lines, "  "+row)
			}
		}
	} else {
		m.recentTableBodyRow = -1
	}

	return lines
}

// padRight pads or truncates s to exactly width visible runes (left-aligned).
func padRight(s string, width int) string {
	vis := lipgloss.Width(s)
	if vis >= width {
		return s
	}
	return s + strings.Repeat(" ", width-vis)
}

// padLeft pads s on the left to exactly width visible runes (right-aligned).
func padLeft(s string, width int) string {
	vis := lipgloss.Width(s)
	if vis >= width {
		return s
	}
	return strings.Repeat(" ", width-vis) + s
}

// truncate clips s to at most n visible runes, appending "…" if clipped.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// toolIcon returns a single unicode icon representing the category of tool.
func toolIcon(tool string) string {
	switch tool {
	case "write_file", "edit_file", "delete_file", "rename_file", "transaction_apply",
		"find_replace", "insert_before_symbol", "insert_after_symbol",
		"replace_symbol_body", "safe_delete_symbol", "rename_symbol":
		return "✎" // write / mutate
	case "read_file", "read_multiple_files", "list_files", "list_directory",
		"find_files", "search_in_files", "file_diff":
		return "▤" // read / browse
	case "find_symbol", "get_definition", "explain_symbol", "list_symbols",
		"workspace_symbols", "find_references", "call_hierarchy", "type_hierarchy":
		return "⊕" // LSP / symbol
	case "write_memory", "read_memory", "delete_memory", "list_memories",
		"search_memories", "relevant_memories":
		return "◎" // memory
	case "git":
		return "◇" // git
	case "diagnostics":
		return "◈" // diagnostics
	case "session_start":
		return "⟳" // session
	default:
		return "▪" // generic
	}
}

// extractCallPath pulls a human-readable file path from a write-tool call's
// InputJSON. Returns "" for non-write tools or empty JSON.
func extractCallPath(tool, inputJSON string) string {
	if inputJSON == "" {
		return ""
	}
	switch tool {
	case "write_file", "edit_file", "delete_file":
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(inputJSON), &a) == nil && a.Path != "" {
			return a.Path
		}
	case "rename_file":
		var a struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if json.Unmarshal([]byte(inputJSON), &a) == nil && a.From != "" {
			return a.From + " → " + a.To
		}
	case "transaction_apply":
		var a struct {
			Operations []struct {
				Path string `json:"path"`
			} `json:"operations"`
		}
		if json.Unmarshal([]byte(inputJSON), &a) == nil {
			return fmt.Sprintf("%d files", len(a.Operations))
		}
	}
	return ""
}

// writeToolSet is the set of tools that mutate files. Used to surface a
// "Recent Edits" panel distinct from query tool activity.
var writeToolSet = map[string]struct{}{
	"write_file":  {},
	"edit_file":   {},
	"delete_file": {},
	"rename_file": {},
}

// filterWriteCalls returns up to n most recent calls whose tool is in
// writeToolSet, preserving the input order (newest-first).
func filterWriteCalls(all []stats.RecentCall, n int) []stats.RecentCall {
	out := make([]stats.RecentCall, 0, n)
	for _, c := range all {
		if _, ok := writeToolSet[c.Tool]; !ok {
			continue
		}
		out = append(out, c)
		if len(out) >= n {
			break
		}
	}
	return out
}

// wrapText breaks s into lines no wider than width, splitting on spaces.
// Words longer than width are kept whole. Newlines in s become spaces.
func wrapText(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	s = strings.ReplaceAll(s, "\n", " ")
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
		} else {
			cur += " " + w
		}
	}
	lines = append(lines, cur)
	return lines
}

func detailRow(key, val string) string {
	return "  " + KeyStyle.Render(key) + ValStyle.Render(val)
}

// contractPath shortens a file path for display:
//  1. Replaces the home directory prefix with ~.
//  2. If still longer than max runes, truncates from the LEFT keeping the tail.
func contractPath(p string, max int) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		p = "~" + p[len(home):]
	}
	runes := []rune(p)
	if len(runes) <= max {
		return p
	}
	if max <= 1 {
		return "…"
	}
	return "…" + string(runes[len(runes)-(max-1):])
}

func humanAgeTUI(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("Jan 2")
	}
}

// daemonRunning reports whether the plumb daemon socket exists. The socket path
// mirrors cli.daemonSocketPath — duplicated here to avoid an import cycle.
// Must stay in sync with cli.plumbRuntimeDir.
func daemonRunning() bool {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	socketPath := filepath.Join(base, "plumb", "plumb.sock")
	_, err = os.Stat(socketPath)
	return err == nil
}

// copyToClipboard returns a Cmd that copies the call's args and output to
// the system clipboard using pbcopy (macOS) or xclip/xsel (Linux).
func copyToClipboard(c stats.RecentCall) tea.Cmd {
	return func() tea.Msg {
		var buf strings.Builder
		if c.InputJSON != "" {
			buf.WriteString("=== Args ===\n")
			var prettyBuf bytes.Buffer
			if err := json.Indent(&prettyBuf, []byte(c.InputJSON), "", "  "); err == nil {
				buf.WriteString(prettyBuf.String())
			} else {
				buf.WriteString(c.InputJSON)
			}
			buf.WriteString("\n")
		}
		if c.OutputText != "" {
			buf.WriteString("=== Output ===\n")
			buf.WriteString(c.OutputText)
			buf.WriteString("\n")
		}
		text := buf.String()
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("pbcopy")
		case "linux":
			if _, err := exec.LookPath("xclip"); err == nil {
				cmd = exec.Command("xclip", "-selection", "clipboard")
			} else {
				cmd = exec.Command("xsel", "--clipboard", "--input")
			}
		}
		if cmd != nil {
			cmd.Stdin = strings.NewReader(text)
			_ = cmd.Run()
		}
		return nil
	}
}

// Run starts the Bubble Tea sessions dashboard.
func Run() error {
	RebuildStyles()
	p := tea.NewProgram(NewModel())
	_, err := p.Run()
	return err
}
