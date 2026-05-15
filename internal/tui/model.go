package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	defaultLeftWidth  = 26
	minLeftWidth      = 16
	minPopupLeftWidth = 30 // enough for " > ● ✓ 05-12 00:00:00 000ms"
	pollInterval      = 2 * time.Second
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
type Model struct {
	sessions        []session.Info
	statsDBs        map[string]*stats.DB
	toolStats       []stats.ToolStat
	recentCalls     []stats.RecentCall
	cursor          int
	statsCursor     int
	toolStatsCursor int
	focusPanel      panelFocus
	leftWidth       int
	width           int
	height          int
	ready           bool
	loadErr         string

	// UI Overlays
	showPopup bool
	showHelp  bool

	popupTool         string
	popupCalls        []stats.RecentCall
	popupCallCursor   int
	popupRightFocus   bool
	popupDetailScroll int
	popupLeftScroll   int
	popupLeftWidth    int
	popupDetail       popupDetailCache

	statsTableBodyRow  int
	recentTableBodyRow int
}

type popupDetailCache struct {
	sessionID  string
	calledAt   int64
	inputJSON  string
	outputText string
	loaded     bool
}

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
	m.sessions = all

	if m.cursor >= len(m.sessions) && m.cursor > 0 {
		m.cursor = len(m.sessions) - 1
	}
	m.refreshStats()
}

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
	m.popupDetail = popupDetailCache{}
}

func (m *Model) openPopup(tool string, preselect time.Time) {
	m.showPopup = true
	m.popupTool = tool
	m.popupCallCursor = 0
	m.popupRightFocus = false
	m.popupDetailScroll = 0
	m.popupLeftScroll = 0
	if m.popupLeftWidth == 0 {
		m.popupLeftWidth = minPopupLeftWidth
	}
	m.refreshPopupCalls()
	if !preselect.IsZero() {
		for i, c := range m.popupCalls {
			if c.CalledAt.Equal(preselect) {
				m.popupCallCursor = i
				break
			}
		}
		m.ensurePopupCursorVisible()
	}
}

func (m *Model) ensurePopupCursorVisible() {
	cursorLine := m.popupCallCursor + 1
	totalLines := len(m.popupCalls) + 1
	bodyHeight := m.height - 6
	if bodyHeight < 1 { bodyHeight = 1 }
	if cursorLine >= m.popupLeftScroll+bodyHeight {
		m.popupLeftScroll = cursorLine - bodyHeight + 1
	}
	if cursorLine < m.popupLeftScroll {
		m.popupLeftScroll = cursorLine
	}
	maxScroll := totalLines - 1
	if maxScroll < 0 { maxScroll = 0 }
	if m.popupLeftScroll > maxScroll { m.popupLeftScroll = maxScroll }
	if m.popupLeftScroll < 0 { m.popupLeftScroll = 0 }
}

func (m *Model) currentDetail() (inputJSON, outputText string) {
	if len(m.popupCalls) == 0 { return }
	c := m.popupCalls[m.popupCallCursor]
	key := c.CalledAt.UnixMilli()
	if m.popupDetail.loaded && m.popupDetail.sessionID == c.SessionID && m.popupDetail.calledAt == key {
		return m.popupDetail.inputJSON, m.popupDetail.outputText
	}
	if len(m.sessions) == 0 { return }
	db := m.dbFor(m.sessions[m.cursor].Folder)
	if db == nil { return }
	inputJSON, outputText = db.CallDetail(c.SessionID, c.CalledAt)
	m.popupDetail = popupDetailCache{
		sessionID:  c.SessionID,
		calledAt:   key,
		inputJSON:  inputJSON,
		outputText: outputText,
		loaded:     true,
	}
	return
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

	case tea.KeyPressMsg:
		if m.showPopup {
			switch msg.String() {
			case "q", "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.showPopup = false
				m.popupCalls = nil
				m.popupDetailScroll = 0
				m.popupLeftScroll = 0
				m.popupRightFocus = false
			case "tab":
				m.popupRightFocus = !m.popupRightFocus
				m.popupDetailScroll = 0
			case "up", "k":
				if m.popupRightFocus {
					if m.popupDetailScroll > 0 { m.popupDetailScroll-- }
				} else {
					if m.popupCallCursor > 0 {
						m.popupCallCursor--
						m.popupDetailScroll = 0
						m.popupDetail = popupDetailCache{}
						m.ensurePopupCursorVisible()
					}
				}
			case "down", "j":
				if m.popupRightFocus {
					m.popupDetailScroll++
				} else {
					if m.popupCallCursor < len(m.popupCalls)-1 {
						m.popupCallCursor++
						m.popupDetailScroll = 0
						m.popupDetail = popupDetailCache{}
						m.ensurePopupCursorVisible()
					}
				}
			case "c":
				if len(m.popupCalls) > 0 {
					inputJSON, outputText := m.currentDetail()
					return m, copyToClipboard(m.popupCalls[m.popupCallCursor], inputJSON, outputText)
				}
			case "[":
				m.popupLeftWidth -= 2
				if m.popupLeftWidth < minPopupLeftWidth { m.popupLeftWidth = minPopupLeftWidth }
			case "]":
				m.popupLeftWidth += 2
				maxPLeft := m.width/2
				if m.popupLeftWidth > maxPLeft { m.popupLeftWidth = maxPLeft }
			case "pgdown":
				pageSize := m.height - 6
				if pageSize < 1 { pageSize = 1 }
				if m.popupRightFocus {
					m.popupDetailScroll += pageSize
				} else {
					m.popupCallCursor += pageSize
					if m.popupCallCursor >= len(m.popupCalls) { m.popupCallCursor = len(m.popupCalls) - 1 }
					m.popupDetailScroll = 0
					m.popupDetail = popupDetailCache{}
					m.ensurePopupCursorVisible()
				}
			case "pgup":
				pageSize := m.height - 6
				if pageSize < 1 { pageSize = 1 }
				if m.popupRightFocus {
					m.popupDetailScroll -= pageSize
					if m.popupDetailScroll < 0 { m.popupDetailScroll = 0 }
				} else {
					m.popupCallCursor -= pageSize
					if m.popupCallCursor < 0 { m.popupCallCursor = 0 }
					m.popupDetailScroll = 0
					m.popupDetail = popupDetailCache{}
					m.ensurePopupCursorVisible()
				}
			}
			return m, nil
		}

		switch msg.String() {
		case "h":
			m.showHelp = true
		case "q", "ctrl+c":
			if m.showHelp {
				m.showHelp = false
				break
			}
			return m, tea.Quit
		case "esc":
			m.showHelp = false
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
			switch m.focusPanel {
			case focusSessions:
				if len(m.toolStats) > 0 { m.focusPanel = focusToolStats } else if len(m.recentCalls) > 0 { m.focusPanel = focusStats }
			case focusToolStats:
				if len(m.recentCalls) > 0 { m.focusPanel = focusStats } else { m.focusPanel = focusSessions }
			case focusStats:
				m.focusPanel = focusSessions
			}
		case "shift+tab":
			switch m.focusPanel {
			case focusSessions:
				if len(m.recentCalls) > 0 { m.focusPanel = focusStats } else if len(m.toolStats) > 0 { m.focusPanel = focusToolStats }
			case focusStats:
				if len(m.toolStats) > 0 { m.focusPanel = focusToolStats } else { m.focusPanel = focusSessions }
			case focusToolStats:
				m.focusPanel = focusSessions
			}
		case "up", "k":
			switch m.focusPanel {
			case focusToolStats:
				if m.toolStatsCursor > 0 { m.toolStatsCursor-- }
			case focusStats:
				if m.statsCursor > 0 { m.statsCursor-- }
			default:
				if m.cursor > 0 {
					m.cursor--
					m.refreshStats()
				}
			}
		case "down", "j":
			switch m.focusPanel {
			case focusToolStats:
				if m.toolStatsCursor < len(m.toolStats)-1 { m.toolStatsCursor++ }
			case focusStats:
				if m.statsCursor < len(m.recentCalls)-1 { m.statsCursor++ }
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
			if m.leftWidth < minLeftWidth { m.leftWidth = minLeftWidth }
		case "]":
			m.leftWidth += 2
			maxLeft := m.width - 23
			if maxLeft < minLeftWidth { maxLeft = minLeftWidth }
			if m.leftWidth > maxLeft { m.leftWidth = maxLeft }
		case "pgdown":
			pageSize := m.height - 6
			if pageSize < 1 { pageSize = 1 }
			switch m.focusPanel {
			case focusToolStats:
				m.toolStatsCursor += pageSize
				if m.toolStatsCursor >= len(m.toolStats) { m.toolStatsCursor = len(m.toolStats) - 1 }
			case focusStats:
				m.statsCursor += pageSize
				if m.statsCursor >= len(m.recentCalls) { m.statsCursor = len(m.recentCalls) - 1 }
			default:
				m.cursor += pageSize
				if m.cursor >= len(m.sessions) { m.cursor = len(m.sessions) - 1 }
				m.refreshStats()
			}
		case "pgup":
			pageSize := m.height - 6
			if pageSize < 1 { pageSize = 1 }
			switch m.focusPanel {
			case focusToolStats:
				m.toolStatsCursor -= pageSize
				if m.toolStatsCursor < 0 { m.toolStatsCursor = 0 }
			case focusStats:
				m.statsCursor -= pageSize
				if m.statsCursor < 0 { m.statsCursor = 0 }
			default:
				m.cursor -= pageSize
				if m.cursor < 0 { m.cursor = 0 }
				m.refreshStats()
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
	if rightWidth < 10 { rightWidth = 10 }
	bodyHeight := m.height - 6
	if bodyHeight < 1 { bodyHeight = 1 }

	var sb strings.Builder
	isOverlay := m.showPopup || m.showHelp
	
	sepStyle := SepStyle
	statusStyle := StatusStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
		statusStyle = InactiveStyle
	}

	// Header: 4-line Logo
	logoLines := strings.Split(LogoText, "\n")
	for i := 0; i < 3; i++ {
		sb.WriteString(sepStyle.Render(padLeft(logoLines[i], m.width)) + "\n")
	}
	sb.WriteString(m.renderTopBorder(rightWidth, isOverlay) + "\n")

	// Body content
	leftLines := m.leftLines()
	rightLines := (&m).rightLines(rightWidth)
	
	for i := range bodyHeight {
		l, r := "", ""
		if i < len(leftLines) { l = leftLines[i] }
		if i < len(rightLines) { r = rightLines[i] }
		
		leftCell := lipgloss.NewStyle().Width(m.leftWidth).Render(l)
		rightCell := lipgloss.NewStyle().Width(rightWidth).Render(r)
		
		if isOverlay {
			lDim := InactiveStyle.Render(stripANSI(leftCell))
			rDim := InactiveStyle.Render(stripANSI(rightCell))
			line := sepStyle.Render("│") + lDim + sepStyle.Render("┆") + rDim + sepStyle.Render("│")
			sb.WriteString(line + "\n")
		} else {
			sb.WriteString(SepStyle.Render("│") + leftCell + SepStyle.Render("┆") + rightCell + SepStyle.Render("│") + "\n")
		}
	}

	sb.WriteString(m.renderBottomBorder(rightWidth, isOverlay) + "\n")

	// Footer
	var totalCalls, savedTok int64
	for _, s := range m.sessions {
		if db := m.dbFor(s.Folder); db != nil {
			totalCalls += db.TotalCalls(stats.Filter{})
			savedTok += db.TotalTokensSaved(stats.Filter{})
		}
	}
	vStr := Version
	if vStr == "" { vStr = "dev" }
	leftFooter := fmt.Sprintf(" plumb %s  ·  %d session(s)  ·  %d tool calls  ·  ~%s tokens saved",
		vStr, len(m.sessions), totalCalls, stats.FormatSavings(int(savedTok)))
	rightFooter := "q quit  ·  h help "
	footerGap := m.width - lipgloss.Width(leftFooter) - lipgloss.Width(rightFooter)
	if footerGap < 1 { footerGap = 1 }
	sb.WriteString(statusStyle.Render(leftFooter + strings.Repeat(" ", footerGap) + rightFooter))

	final := sb.String()
	if m.showPopup {
		final = m.renderPopup(final, rightWidth, bodyHeight)
	}
	if m.showHelp {
		final = m.renderHelp(final)
	}
	return final
}

func (m Model) renderTopBorder(rightWidth int, dimmed bool) string {
	var leftTitle, rightTitle string
	var leftStyle, rightStyle lipgloss.Style
	sepStyle := SepStyle
	
	if dimmed {
		sepStyle = SepInactiveStyle
		leftStyle = PanelHeaderInactiveStyle
		rightStyle = PanelHeaderInactiveStyle
	} else {
		if m.focusPanel == focusSessions {
			leftStyle, rightStyle = PanelHeaderStyle, PanelHeaderFadedStyle
		} else {
			leftStyle, rightStyle = PanelHeaderFadedStyle, PanelHeaderStyle
		}
	}

	leftTitle = fmt.Sprintf(" Sessions (%d) ", len(m.sessions))
	rightTitle = " Session + Stats "

	leftPart := sepStyle.Render("╭─") + leftStyle.Render(leftTitle)
	leftFill := m.leftWidth - 1 - len(leftTitle)
	if leftFill < 0 { leftFill = 0 }
	midPart := sepStyle.Render(strings.Repeat("─", leftFill)+"┬─") + rightStyle.Render(rightTitle)
	
	logoBottom := strings.Split(LogoText, "\n")[3]
	currentW := lipgloss.Width(leftPart) + lipgloss.Width(midPart)
	fillerW := m.width - currentW - LogoWidth
	if fillerW < 0 { fillerW = 0 }
	
	return leftPart + midPart + sepStyle.Render(strings.Repeat("─", fillerW)) + sepStyle.Render(logoBottom)
}

func (m Model) renderBottomBorder(rightWidth int, dimmed bool) string {
	sepStyle := SepStyle
	if dimmed {
		sepStyle = SepInactiveStyle
	}
	return sepStyle.Render("╰" + strings.Repeat("─", m.leftWidth) + "┴" + strings.Repeat("─", rightWidth) + "╯")
}

func (m Model) renderPopup(bg string, rightWidth, bodyHeight int) string {
	if m.popupLeftWidth == 0 { m.popupLeftWidth = minPopupLeftWidth }
	pLW, pRW := m.popupLeftWidth, m.width - m.popupLeftWidth - 3
	if pRW < 10 { pRW = 10 }

	var lines []string
	lines = append(lines, m.renderTopBorderPopup(pLW, pRW))
	allLeft := m.popupLeftLines()
	visibleLeft := allLeft[m.popupLeftScroll:]
	leftScrollbar := scrollbarCol(len(allLeft), bodyHeight, m.popupLeftScroll)
	allRight := m.popupRightAll(pRW - 2)
	maxDS := len(allRight) - bodyHeight
	if maxDS < 0 { maxDS = 0 }
	if m.popupDetailScroll > maxDS { m.popupDetailScroll = maxDS }
	visibleRight := allRight[m.popupDetailScroll:]
	scrollbar := scrollbarCol(len(allRight), bodyHeight, m.popupDetailScroll)

	for i := range bodyHeight {
		var lCell string
		if i < len(visibleLeft) && visibleLeft[i] != "" {
			lCell = lipgloss.NewStyle().Width(pLW).Render(visibleLeft[i])
		} else {
			lCell = lipgloss.NewStyle().Width(pLW).Render("")
		}
		var rStr string
		if i < len(visibleRight) { rStr = visibleRight[i] }
		rCell := lipgloss.NewStyle().Width(pRW).Render(rStr)
		
		lb := SepStyle.Render("│")
		if leftScrollbar != nil && i < len(leftScrollbar) { lb = leftScrollbar[i] }
		rb := SepStyle.Render("│")
		if scrollbar != nil && i < len(scrollbar) { rb = scrollbar[i] }
		
		lines = append(lines, lb + lCell + SepStyle.Render("┆") + rCell + rb)
	}
	lines = append(lines, m.renderBottomBorderPopup(pLW, pRW))
	return spliceOverlay(bg, strings.Join(lines, "\n"), m.width, m.height)
}

func (m Model) renderHelp(bg string) string {
	helpLines := []string{
		" ↑/↓ or j/k     Navigate sessions/calls",
		" pgup/pgdown    Page through lists",
		" tab/shift+tab  Switch panel focus",
		" enter          Open tool call detail",
		" [/]            Resize columns",
		" q              Quit",
		" esc            Close help",
	}
	
	boxW := 44
	innerW := boxW - 2
	topLabel := " Help & Navigation "
	
	top := SepStyle.Render("╭─") + PanelHeaderStyle.Render(topLabel) + SepStyle.Render(strings.Repeat("─", innerW-len(topLabel)-1)+"╮")
	
	var bodyLines []string
	// Empty row top
	bodyLines = append(bodyLines, SepStyle.Render("│") + strings.Repeat(" ", innerW) + SepStyle.Render("│"))
	
	for _, l := range helpLines {
		content := "  " + padRight(l, innerW-4) + "  "
		bodyLines = append(bodyLines, SepStyle.Render("│") + content + SepStyle.Render("│"))
	}
	
	// Empty row bottom
	bodyLines = append(bodyLines, SepStyle.Render("│") + strings.Repeat(" ", innerW) + SepStyle.Render("│"))
	
	bottom := SepStyle.Render("╰" + strings.Repeat("─", innerW) + "╯")
	
	popup := top + "\n" + strings.Join(bodyLines, "\n") + "\n" + bottom
	return spliceOverlay(bg, popup, m.width, m.height)
}

func (m Model) renderTopBorderPopup(pLW, pRW int) string {
	lt, rt := " Timestamp ", " Call Detail "
	var lts, rts lipgloss.Style
	if m.popupRightFocus { lts, rts = PanelHeaderFadedStyle, PanelHeaderStyle } else { lts, rts = PanelHeaderStyle, PanelHeaderFadedStyle }
	lf := pLW - 1 - len(lt)
	if lf < 0 { lf = 0 }
	rf := pRW - 1 - len(rt)
	if rf < 0 { rf = 0 }
	return SepStyle.Render("╭─") + lts.Render(lt) + SepStyle.Render(strings.Repeat("─", lf)+"┬─") + rts.Render(rt) + SepStyle.Render(strings.Repeat("─", rf)+"╮")
}

func (m Model) renderBottomBorderPopup(pLW, pRW int) string {
	return SepStyle.Render("╰" + strings.Repeat("─", pLW) + "┴" + strings.Repeat("─", pRW) + "╯")
}

func (m Model) popupLeftLines() []string {
	lines := []string{""}
	if len(m.popupCalls) == 0 {
		lines = append(lines, "   "+MutedStyle.Render("No calls recorded yet."))
		return lines
	}
	var currID string
	if len(m.sessions) > 0 { currID = m.sessions[m.cursor].ID }
	for i, c := range m.popupCalls {
		sel := !m.popupRightFocus && i == m.popupCallCursor
		pre := "   "
		if i == m.popupCallCursor { pre = " > " }
		sc := "○"
		if c.SessionID == currID { sc = "●" }
		ok := "✓"
		if !c.Success { ok = "✗" }
		ts := c.CalledAt.Format("01-02 15:04:05")
		dur := fmt.Sprintf("%dms", c.DurationMs)
		row := fmt.Sprintf("%s%s %s %s %s", pre, sc, ok, ts, dur)
		maxW := m.popupLeftWidth - 1
		if maxW < 10 { maxW = 10 }
		if lipgloss.Width(row) > maxW { row = string([]rune(row)[:maxW-1]) + "…" }
		if sel { lines = append(lines, SelectedStyle.Render(row)) } else if m.popupRightFocus { lines = append(lines, TimestampFadedStyle.Render(row)) } else if !c.Success {
			p1 := fmt.Sprintf("%s%s ", pre, sc)
			err := WarnStyle.Render("✗")
			p2 := fmt.Sprintf(" %s %s", ts, dur)
			lines = append(lines, TimestampActiveStyle.Render(p1) + err + TimestampActiveStyle.Render(p2))
		} else { lines = append(lines, TimestampActiveStyle.Render(row)) }
	}
	return lines
}

func (m Model) popupRightAll(rw int) []string {
	lines := []string{""}
	if len(m.popupCalls) == 0 {
		lines = append(lines, "  "+MutedStyle.Render("No calls to show."))
		return lines
	}
	c := m.popupCalls[m.popupCallCursor]
	var currID string
	if len(m.sessions) > 0 { currID = m.sessions[m.cursor].ID }
	st := OkStyle.Render("✓ success")
	if !c.Success { st = WarnStyle.Render("✗ failed") }
	sl := MutedStyle.Render("○ historical")
	if c.SessionID == currID { sl = OkStyle.Render("● current") }
	sID := c.SessionID
	if len(sID) > 12 { sID = sID[:12] + "…" }
	lines = append(lines, detailRow("Tool", DetailStyle.Render(c.Tool)), detailRow("Status", st), detailRow("Called at", DetailStyle.Render(c.CalledAt.Format("2006-01-02 15:04:05"))), detailRow("Session", sID+"  "+sl), detailRow("Duration", DetailStyle.Render(fmt.Sprintf("%d ms", c.DurationMs))), detailRow("Input", DetailStyle.Render(fmt.Sprintf("%d bytes", c.InputBytes))), detailRow("Output", DetailStyle.Render(fmt.Sprintf("%d bytes", c.OutputBytes))))
	bx := func(label string, content []string) {
		inner := rw - 4
		if inner < 8 { inner = 8 }
		tl := " " + label + " "
		tf := inner + 1 - len(tl)
		if tf < 0 { tf = 0 }
		lines = append(lines, "", " "+SepStyle.Render("╭─")+PanelHeaderStyle.Render(tl)+SepStyle.Render(strings.Repeat("─", tf)+"╮"))
		for _, cl := range content {
			if lipgloss.Width(cl) > inner { cl = string([]rune(cl)[:inner-1]) + "…" }
			p := lipgloss.NewStyle().Width(inner).Render(cl)
			lines = append(lines, " "+SepStyle.Render("│")+" "+p+" "+SepStyle.Render("│"))
		}
		lines = append(lines, " "+SepStyle.Render("╰"+strings.Repeat("─", inner+2)+"╯"))
	}
	if !c.Success {
		var el []string
		if c.ErrorMsg != "" { for _, w := range wrapText(c.ErrorMsg, rw-5) { el = append(el, WarnStyle.Render(w)) } } else { el = append(el, MutedStyle.Render("(no error message recorded)")) }
		bx("Error", el)
	}
	ij, ot := m.currentDetail()
	if ij != "" {
		var al []string
		var pb bytes.Buffer
		if err := json.Indent(&pb, []byte(ij), "", "  "); err == nil {
			for _, l := range strings.Split(strings.TrimRight(pb.String(), "\n"), "\n") { al = append(al, DetailStyle.Render(l)) }
		} else { al = append(al, DetailStyle.Render(ij)) }
		bx("Args", al)
	}
	if ot != "" && c.Success {
		var ol []string
		for _, o := range strings.Split(strings.TrimRight(ot, "\n"), "\n") { ol = append(ol, DetailStyle.Render(o)) }
		bx("Output", ol)
	}
	if m.popupRightFocus { lines = append(lines, "", "  "+MutedStyle.Render("c copy · tab back")) }
	return lines
}

func scrollbarCol(total, visible, offset int) []string {
	if total <= visible { return nil }
	ts := visible * visible / total
	if ts < 1 { ts = 1 }
	mo := total - visible
	if mo < 1 { mo = 1 }
	tst := offset * (visible - ts) / mo
	col := make([]string, visible)
	for i := range visible {
		if i >= tst && i < tst+ts { col[i] = ScrollThumbStyle.Render("┃") } else { col[i] = ScrollTrackStyle.Render("│") }
	}
	return col
}

func (m Model) leftLines() []string {
	lf := m.focusPanel == focusSessions
	lines := []string{""}
	if len(m.sessions) == 0 {
		m1, m2 := " Daemon running.", " Call a tool to begin."
		if !daemonRunning() { m1, m2 = " No active sessions.", "" }
		if lf { lines = append(lines, MutedStyle.Render(m1)); if m2 != "" { lines = append(lines, MutedStyle.Render(m2)) }
		} else { lines = append(lines, InactiveStyle.Render(m1)); if m2 != "" { lines = append(lines, InactiveStyle.Render(m2)) } }
		return lines
	}
	for i, s := range m.sessions {
		var label string
		if s.Folder == "" { label = "⟳ resolving…" } else {
			lp := ""
			if s.Language != "" && s.Language != "none" { lp = s.Language + ": " }
			mf := m.leftWidth - 4 - len([]rune(lp))
			if mf < 0 { mf = 0 }
			label = lp + contractPath(s.Folder, mf)
		}
		if i == m.cursor {
			if lf { lines = append(lines, SelectedStyle.Render(" > "+label)) } else { lines = append(lines, FadedStyle.Render(" > "+label)) }
		} else {
			if lf { lines = append(lines, ItemStyle.Render("   "+label)) } else { lines = append(lines, FadedStyle.Render("   "+label)) }
		}
	}
	return lines
}

func (m *Model) handleRightPanelClick(bodyRow int) {
	if m.statsTableBodyRow >= 0 && len(m.toolStats) > 0 {
		idx := bodyRow - m.statsTableBodyRow
		if idx >= 0 && idx < len(m.toolStats) { m.toolStatsCursor, m.focusPanel = idx, focusToolStats; return }
	}
	if m.recentTableBodyRow >= 0 && len(m.recentCalls) > 0 {
		idx := bodyRow - m.recentTableBodyRow
		if idx >= 0 && idx < len(m.recentCalls) { m.statsCursor, m.focusPanel = idx, focusStats; return }
	}
}

func (m *Model) rightLines(rw int) []string {
	lines := []string{""}
	if len(m.sessions) == 0 {
		lines = append(lines, "  "+MutedStyle.Render("Select a session to view details."))
		return lines
	}
	const kw = 14
	mv := rw - kw
	if mv < 8 { mv = 8 }
	s := m.sessions[m.cursor]
	fld := s.Folder
	if fld == "" { fld = MutedStyle.Render("(resolving workspace…)") } else { fld = contractPath(fld, mv) }
	adp := s.Adapter
	if adp == "" { adp = "—" }
	lines = append(lines, detailRow("ID", s.ID), detailRow("Language", s.Language), detailRow("Folder", fld), detailRow("Adapter", adp), detailRow("PID", fmt.Sprintf("%d", s.PID)))
	if s.DaemonVersion != "" { lines = append(lines, detailRow("Daemon", s.DaemonVersion)) }
	lines = append(lines, detailRow("Started", s.StartedAt.Format("2006-01-02 15:04:05")))
	cl := s.ClientName
	if s.ClientVersion != "" { cl += " " + s.ClientVersion }
	if cl == "" { cl = MutedStyle.Render("unknown") }
	lines = append(lines, detailRow("Client", cl))
	const (c2w, c3w, c4w = 8, 10, 6)
	s3 := "   "
	c1w := rw - 2 - c2w - c3w - c4w - 12
	if c1w < 10 { c1w = 10 }
	sln := "  " + SepStyle.Render(strings.Repeat("─", rw-3))
	roww := rw - 2
	lines = append(lines, "")
	if len(m.toolStats) == 0 {
		if m.focusPanel == focusToolStats { lines = append(lines, "  "+SelectedStyle.Render("Tools")) } else { lines = append(lines, "  "+HintStyle.Render("Tools")) }
		lines = append(lines, "  "+MutedStyle.Render("No calls recorded yet."))
		m.statsTableBodyRow = -1
	} else {
		var lc string
		if m.focusPanel == focusToolStats { lc = padRight(SelectedStyle.Render("Tools"), c1w) } else { lc = padRight(HintStyle.Render("Tools"), c1w) }
		h := "  " + lc + s3 + padLeft(HintStyle.Render("Calls"), c2w) + s3 + padLeft(HintStyle.Render("Avg"), c3w) + s3 + HintStyle.Render("Errors")
		lines = append(lines, h, sln)
		m.statsTableBodyRow = len(lines)
		for i, ts := range m.toolStats {
			sel := m.focusPanel == focusToolStats && i == m.toolStatsCursor
			tn := padRight(truncate(ts.Tool, c1w-2), c1w-2)
			if sel {
				pc, pa, pe := padLeft(fmt.Sprintf("%d", ts.Calls), c2w), padLeft(fmt.Sprintf("%.0fms", ts.AvgMs), c3w), padLeft("", c4w)
				if ts.Errors > 0 { pe = padLeft(fmt.Sprintf("%d", ts.Errors), c4w) }
				lines = append(lines, SelectedStyle.Width(roww).Render("  > "+tn+s3+pc+s3+pa+s3+pe+s3))
			} else {
				c2, c3, c4 := padLeft(OkStyle.Render(fmt.Sprintf("%d", ts.Calls)), c2w), padLeft(MutedStyle.Render(fmt.Sprintf("%.0fms", ts.AvgMs)), c3w), padLeft("", c4w)
				if ts.Errors > 0 { c4 = padLeft(WarnStyle.Render(fmt.Sprintf("%d", ts.Errors)), c4w) }
				lines = append(lines, "  ○ "+tn+s3+c2+s3+c3+s3+c4+s3)
			}
		}
	}
	if len(m.recentCalls) > 0 {
		lines = append(lines, "")
		var rlc string
		if m.focusPanel == focusStats { rlc = padRight(SelectedStyle.Render("Recent Tools"), c1w) } else { rlc = padRight(HintStyle.Render("Recent Tools"), c1w) }
		h := "  " + rlc + s3 + padLeft(HintStyle.Render("Dur"), c2w) + s3 + padLeft(HintStyle.Render("When"), c3w) + s3 + HintStyle.Render("Errors")
		lines = append(lines, h, sln)
		m.recentTableBodyRow = len(lines)
		for i, c := range m.recentCalls {
			sel := m.focusPanel == focusStats && i == m.statsCursor
			tn := padRight(truncate(c.Tool, c1w-2), c1w-2)
			if sel {
				pd, pw, pe := padLeft(fmt.Sprintf("%dms", c.DurationMs), c2w), padLeft(humanAgeTUI(c.CalledAt), c3w), padLeft("", c4w)
				if !c.Success { pe = padLeft("✗", c4w) }
				lines = append(lines, SelectedStyle.Width(roww).Render("  > "+tn+s3+pd+s3+pw+s3+pe+s3))
			} else {
				mk := OkStyle.Render("✓") + " "
				if !c.Success { mk = WarnStyle.Render("✗") + " " }
				c2, c3, c4 := padLeft(MutedStyle.Render(fmt.Sprintf("%dms", c.DurationMs)), c2w), padLeft(MutedStyle.Render(humanAgeTUI(c.CalledAt)), c3w), padLeft("", c4w)
				if !c.Success { c4 = padLeft(WarnStyle.Render("✗"), c4w) }
				lines = append(lines, "  "+mk+tn+s3+c2+s3+c3+s3+c4+s3)
			}
		}
	} else { m.recentTableBodyRow = -1 }
	return lines
}

func padRight(s string, w int) string {
	v := lipgloss.Width(s)
	if v >= w { return s }
	return s + strings.Repeat(" ", w-v)
}

func padLeft(s string, w int) string {
	v := lipgloss.Width(s)
	if v >= w { return s }
	return strings.Repeat(" ", w-v) + s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n { return s }
	if n <= 1 { return "…" }
	return string(r[:n-1]) + "…"
}

func wrapText(s string, width int) []string {
	if width < 8 { width = 8 }
	s = strings.ReplaceAll(s, "\n", " ")
	words := strings.Fields(s)
	if len(words) == 0 { return nil }
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
	return append(lines, cur)
}

func detailRow(k, v string) string { return "  " + KeyStyle.Render(k) + ValStyle.Render(v) }

func contractPath(p string, max int) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) { p = "~" + p[len(h):] }
	r := []rune(p)
	if len(r) <= max { return p }
	if max <= 1 { return "…" }
	return "…" + string(r[len(r)-(max-1):])
}

func humanAgeTUI(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute: return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour: return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour: return fmt.Sprintf("%dh ago", int(d.Hours()))
	default: return t.Format("Jan 2")
	}
}

func daemonRunning() bool {
	base, err := os.UserCacheDir()
	if err != nil { base = os.TempDir() }
	_, err = os.Stat(filepath.Join(base, "plumb", "plumb.sock"))
	return err == nil
}

func copyToClipboard(c stats.RecentCall, ij, ot string) tea.Cmd {
	return func() tea.Msg {
		var buf strings.Builder
		if ij != "" {
			buf.WriteString("=== Args ===\n")
			var pb bytes.Buffer
			if err := json.Indent(&pb, []byte(ij), "", "  "); err == nil {
				buf.WriteString(pb.String())
			} else {
				buf.WriteString(ij)
			}
			buf.WriteString("\n")
		}
		if ot != "" { buf.WriteString("=== Output ===\n"); buf.WriteString(ot); buf.WriteString("\n") }
		txt := buf.String()
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin": cmd = exec.Command("pbcopy")
		case "linux":
			if _, err := exec.LookPath("xclip"); err == nil { cmd = exec.Command("xclip", "-selection", "clipboard") } else { cmd = exec.Command("xsel", "--clipboard", "--input") }
		}
		if cmd != nil { cmd.Stdin = strings.NewReader(txt); _ = cmd.Run() }
		return nil
	}
}

func spliceOverlay(bg, overlay string, w, h int) string {
	bgLines := strings.Split(bg, "\n")
	ovLines := strings.Split(overlay, "\n")
	ovH := len(ovLines)
	ovW := 0
	for _, l := range ovLines {
		if lw := lipgloss.Width(l); lw > ovW { ovW = lw }
	}
	sy, sx := (h-ovH)/2, (w-ovW)/2
	for i := 0; i < ovH; i++ {
		y := sy + i
		if y < 0 || y >= len(bgLines) { continue }
		bl, ol := bgLines[y], ovLines[i]
		blW := lipgloss.Width(bl)
		if sx >= blW { continue }
		// Crude but effective splice: strip ANSI from background to calculate positions,
		// then re-assemble. For better modal logic, we treat the background as 
		// "already dimmed" text.
		rawBG := []rune(stripANSI(bl))
		prefix := ""
		if sx > 0 {
			if sx < len(rawBG) { prefix = string(rawBG[:sx]) } else { prefix = string(rawBG) }
		}
		suffix := ""
		if sx+ovW < len(rawBG) { suffix = string(rawBG[sx+ovW:]) }
		
		bgLines[y] = InactiveStyle.Render(prefix) + ol + InactiveStyle.Render(suffix)
	}
	return strings.Join(bgLines, "\n")
}

var ansiRegex = regexp.MustCompile("[\u001b\u009b][[()#;?]*(?:[0-9]{1,4}(?:;[0-9]{0,4})*)?[0-9A-ORZcf-nqry=><]")

func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

func Run() error {
	RebuildStyles()
	p := tea.NewProgram(NewModel())
	_, err := p.Run()
	return err
}
