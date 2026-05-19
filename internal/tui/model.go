package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/golimpio/plumb/internal/monitor"
	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/stats"
)

// Version is set by the cli package before calling Run so it appears in the header.
var Version string

const (
	defaultLeftWidth  = 30
	minLeftWidth      = 20
	minPopupLeftWidth = 30 // enough for " > ● ✓ 05-12 00:00:00 000ms"
	sectionMenuWidth  = 22
	pollInterval      = 2 * time.Second
	activityInterval  = 10 * time.Second
	activityBuckets   = 16
	bodyStartRow      = 4
)

var sectionMenuItems = []string{"Dashboard", "Sessions", "Memory", "Logs", "Settings"}

// pollMsg is sent by the periodic refresh tick.
type pollMsg struct{}

// panelFocus identifies which panel / section consumes navigation keys.
type panelFocus int

const (
	focusSessions    panelFocus = iota // j/k moves the session cursor (default)
	focusToolStats                     // j/k moves the Tool Statistics cursor
	focusStats                         // j/k moves the Recent calls cursor
	focusDetails                       // j/k scrolls the Details panel
	focusDiagnostics                   // j/k scrolls the Diagnostics panel
	focusLogs                          // j/k scrolls the log viewer (Logs section)
)

// Model is the root Bubble Tea model for the sessions dashboard.
type Model struct {
	sessions        []session.Info
	globalDB        *stats.DB
	toolStats       []stats.ToolStat
	recentCalls     []stats.RecentCall
	activity        stats.ActivitySummary
	tokenSavings    int64
	daemonMetrics   monitor.DaemonMetrics
	daemonMetricsOK bool
	daemonCPU       []float64
	cursor          int
	statsCursor     int
	toolStatsCursor int
	focusPanel      panelFocus
	leftScroll      int
	rightScroll     int
	leftWidth       int
	width           int
	height          int
	ready           bool
	draggingDivider bool
	lastActivityAt  time.Time
	activitySession string // DEPRECATED: no longer used for activity caching since it's global
	loadErr         string

	// UI Overlays
	showPopup bool
	showHelp  bool

	sectionMenuOpen   bool
	sectionMenuCursor int
	currentSection    int
	rightTab          int // 0=Details 1=Tools 2=History 3=Diagnostics

	popupTool         string
	popupCalls        []stats.RecentCall
	popupCallCursor   int
	popupRightFocus   bool
	popupDetailScroll int
	popupLeftScroll   int
	popupLeftWidth    int
	popupDetail       popupDetailCache

	statsTableBodyRow      int
	recentTableBodyRow     int
	lastDiagnosticsOutput  string

	// Log viewer state (Logs section, index 3).
	logPath    string
	logEntries []logEntry
	logFilter  string
	logScroll  int
	logCursor  int
	logOffset  int64
	logFollow  bool // true = auto-scroll to the newest entry
	logInitd   bool // true = initLogTail has been called

	logDetailOpen   bool
	logDetailScroll int
	logDetailCopied bool
}

type logDetailCopyResetMsg struct{}

type popupDetailCache struct {
	sessionID  string
	calledAt   int64
	inputJSON  string
	outputText string
	loaded     bool
}

func NewModel(logPath string) Model {
	m := Model{
		leftWidth:         defaultLeftWidth,
		currentSection:    1,
		sectionMenuCursor: 1,
		logPath:           logPath,
		logFollow:         true,
	}
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
	m.refreshDaemonMetrics()
	m.refreshStats()
}

func (m *Model) refreshDaemonMetrics() {
	metrics, err := monitor.ReadSnapshot(monitor.SnapshotPath())
	if err != nil || metrics.SampledAt.IsZero() || time.Since(metrics.SampledAt) > 10*time.Second {
		m.daemonMetricsOK = false
		return
	}
	m.daemonMetrics = metrics
	m.daemonMetricsOK = true
	if metrics.CPUAvailable {
		m.daemonCPU = append(m.daemonCPU, clampPercent(metrics.CPUPercent))
		if len(m.daemonCPU) > activityBuckets {
			m.daemonCPU = m.daemonCPU[len(m.daemonCPU)-activityBuckets:]
		}
	}
}

func (m *Model) dbFor(workspace string) *stats.DB {
	// DEPRECATED: stats are now global.
	return m.globalDB
}

func (m *Model) refreshStats() {
	if m.globalDB == nil {
		m.globalDB, _ = stats.OpenReadOnly()
	}
	if m.globalDB == nil {
		m.toolStats = nil
		m.recentCalls = nil
		m.activity = stats.ActivitySummary{}
		m.tokenSavings = 0
		return
	}
	if len(m.sessions) == 0 {
		m.toolStats = nil
		m.recentCalls = nil
		m.activity = stats.ActivitySummary{}
		m.tokenSavings = 0
		return
	}
	s := m.sessions[m.cursor]

	var prevTool string
	if m.toolStatsCursor < len(m.toolStats) {
		prevTool = m.toolStats[m.toolStatsCursor].Tool
	}
	prevCall := selectedCallKey(m.recentCalls, m.statsCursor)

	filter := stats.Filter{Workspace: s.Folder, SessionID: s.ID}
	m.toolStats, _ = m.globalDB.Summary(filter)
	m.recentCalls, _ = m.globalDB.Recent(50, filter)
	m.refreshActivity(m.globalDB, time.Now())

	m.statsCursor = locateCall(m.recentCalls, prevCall, m.statsCursor)
	m.toolStatsCursor = locateTool(m.toolStats, prevTool, m.toolStatsCursor)
	m.refreshDiagnostics()
}

func (m *Model) refreshDiagnostics() {
	if m.globalDB == nil || len(m.sessions) == 0 {
		m.lastDiagnosticsOutput = ""
		return
	}
	s := m.sessions[m.cursor]
	calls, _ := m.globalDB.CallsForTool("diagnostics", s.Folder, 1)
	if len(calls) == 0 {
		m.lastDiagnosticsOutput = ""
		return
	}
	_, output := m.globalDB.CallDetail(calls[0].Workspace, calls[0].SessionID, calls[0].CalledAt)
	m.lastDiagnosticsOutput = output
}

func (m *Model) refreshActivity(db *stats.DB, now time.Time) {
	if db == nil {
		m.activity = stats.ActivitySummary{}
		m.tokenSavings = 0
		return
	}
	// We no longer tie caching to activitySession because the activity view is global.
	if !m.lastActivityAt.IsZero() && now.Sub(m.lastActivityAt) < activityInterval {
		return
	}

	var start time.Time
	if len(m.sessions) > 0 {
		start = m.sessions[0].StartedAt
	}
	if start.IsZero() {
		start = now.Add(-time.Minute)
	}
	window := now.Sub(start)
	if window < time.Minute {
		window = time.Minute
	}

	// Pass an empty stats.Filter{} to get ALL calls, regardless of session or tool.
	activity, err := db.Activity(window, activityBuckets, stats.Filter{})
	if err != nil {
		m.activity = stats.ActivitySummary{}
		return
	}
	m.activity = activity
	m.tokenSavings = db.TotalTokensSavedSince(start, stats.Filter{})
	m.lastActivityAt = now
	m.activitySession = "" // clear to prevent any residual checks
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
	// Open global DB if not already open.
	if m.globalDB == nil {
		m.globalDB, _ = stats.OpenReadOnly()
	}
	if m.globalDB == nil {
		m.popupCalls = nil
		return
	}

	prev := selectedCallKey(m.popupCalls, m.popupCallCursor)
	// We want calls for this tool in this workspace.
	ws := m.sessions[m.cursor].Folder
	m.popupCalls, _ = m.globalDB.CallsForTool(m.popupTool, ws, 200)
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
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	if cursorLine >= m.popupLeftScroll+bodyHeight {
		m.popupLeftScroll = cursorLine - bodyHeight + 1
	}
	if cursorLine < m.popupLeftScroll {
		m.popupLeftScroll = cursorLine
	}
	maxScroll := totalLines - 1
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.popupLeftScroll > maxScroll {
		m.popupLeftScroll = maxScroll
	}
	if m.popupLeftScroll < 0 {
		m.popupLeftScroll = 0
	}
}

func (m *Model) currentDetail() (inputJSON, outputText string) {
	if len(m.popupCalls) == 0 {
		return
	}
	c := m.popupCalls[m.popupCallCursor]
	key := c.CalledAt.UnixMilli()
	if m.popupDetail.loaded && m.popupDetail.sessionID == c.SessionID && m.popupDetail.calledAt == key {
		return m.popupDetail.inputJSON, m.popupDetail.outputText
	}
	if len(m.sessions) == 0 {
		return
	}
	// Open global DB if not already open.
	if m.globalDB == nil {
		m.globalDB, _ = stats.OpenReadOnly()
	}
	if m.globalDB == nil {
		return
	}
	inputJSON, outputText = m.globalDB.CallDetail(c.Workspace, c.SessionID, c.CalledAt)
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
		if m.currentSection == 3 && m.logInitd {
			newEntries, newOffset := readNewLogLines(m.logPath, m.logOffset)
			if len(newEntries) > 0 {
				m.logOffset = newOffset
				m.logEntries = append(m.logEntries, newEntries...)
				if len(m.logEntries) > maxLogEntries {
					m.logEntries = m.logEntries[len(m.logEntries)-maxLogEntries:]
				}
			}
		}
		return m, tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })

	case logDetailCopyResetMsg:
		m.logDetailCopied = false
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true

	case tea.MouseClickMsg:
		mouse := msg.Mouse()
		if mouse.Button == tea.MouseLeft {
			if m.logDetailOpen {
				return m, nil
			}
			if m.onSectionSelector(mouse.X, mouse.Y) {
				m.sectionMenuOpen = true
				m.sectionMenuCursor = m.currentSection
			} else if m.sectionMenuOpen {
				m.selectSectionMenuAtRow(mouse.Y)
			} else if m.currentSection == 3 && !m.showHelp {
				m.selectLogAtBodyRow(mouse.Y - bodyStartRow)
			} else if m.onDivider(mouse.X) {
				m.draggingDivider = true
				m.setLeftWidthFromMouse(mouse.X)
			} else if m.onSessionsPanel(mouse.X, mouse.Y) {
				m.selectSessionAtBodyRow(mouse.Y - bodyStartRow)
			}
		}

	case tea.MouseWheelMsg:
		mouse := msg.Mouse()
		switch mouse.Button {
		case tea.MouseWheelUp:
			m.handleMouseWheel(mouse, -3)
		case tea.MouseWheelDown:
			m.handleMouseWheel(mouse, 3)
		}

	case tea.MouseMotionMsg:
		mouse := msg.Mouse()
		if m.draggingDivider && mouse.Button == tea.MouseLeft {
			m.setLeftWidthFromMouse(mouse.X)
		}

	case tea.MouseReleaseMsg:
		m.draggingDivider = false

	case tea.KeyPressMsg:
		if m.showPopup {
			switch msg.String() {
			case "ctrl+q", "ctrl+c":
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
					if m.popupDetailScroll > 0 {
						m.popupDetailScroll--
					}
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
				if m.popupLeftWidth < minPopupLeftWidth {
					m.popupLeftWidth = minPopupLeftWidth
				}
			case "]":
				m.popupLeftWidth += 2
				maxPLeft := m.width / 2
				if m.popupLeftWidth > maxPLeft {
					m.popupLeftWidth = maxPLeft
				}
			case "pgdown":
				pageSize := m.height - 6
				if pageSize < 1 {
					pageSize = 1
				}
				if m.popupRightFocus {
					m.popupDetailScroll += pageSize
				} else {
					m.popupCallCursor += pageSize
					if m.popupCallCursor >= len(m.popupCalls) {
						m.popupCallCursor = len(m.popupCalls) - 1
					}
					m.popupDetailScroll = 0
					m.popupDetail = popupDetailCache{}
					m.ensurePopupCursorVisible()
				}
			case "pgup":
				pageSize := m.height - 6
				if pageSize < 1 {
					pageSize = 1
				}
				if m.popupRightFocus {
					m.popupDetailScroll -= pageSize
					if m.popupDetailScroll < 0 {
						m.popupDetailScroll = 0
					}
				} else {
					m.popupCallCursor -= pageSize
					if m.popupCallCursor < 0 {
						m.popupCallCursor = 0
					}
					m.popupDetailScroll = 0
					m.popupDetail = popupDetailCache{}
					m.ensurePopupCursorVisible()
				}
			}
			return m, nil
		}

		// Logs section intercepts keys when the menu/help overlay is not open.
		if m.currentSection == 3 && !m.sectionMenuOpen && !m.showHelp {
			if m.logDetailOpen {
				switch msg.String() {
				case "ctrl+q", "ctrl+c":
					return m, tea.Quit
				case "esc":
					m.logDetailOpen = false
					m.logDetailScroll = 0
				case "c":
					if text := m.currentLogDetailText(); text != "" {
						m.logDetailCopied = true
						return m, tea.Batch(
							copyTextToClipboard(text),
							tea.Tick(3*time.Second, func(time.Time) tea.Msg { return logDetailCopyResetMsg{} }),
						)
					}
				case "up", "k":
					if m.logDetailScroll > 0 {
						m.logDetailScroll--
					}
				case "down", "j":
					m.logDetailScroll++
				case "pgup":
					pageSize := m.height - 6
					if pageSize < 1 {
						pageSize = 1
					}
					m.logDetailScroll -= pageSize
					if m.logDetailScroll < 0 {
						m.logDetailScroll = 0
					}
				case "pgdown":
					pageSize := m.height - 6
					if pageSize < 1 {
						pageSize = 1
					}
					m.logDetailScroll += pageSize
				}
				return m, nil
			}
			switch msg.String() {
			case "ctrl+q", "ctrl+c":
				return m, tea.Quit
			case "esc":
				if m.logFilter != "" {
					m.logFilter = ""
					m.logScroll = 0
				} else {
					m.sectionMenuOpen = true
					m.sectionMenuCursor = m.currentSection
				}
			case "/":
				m.sectionMenuOpen = true
				m.sectionMenuCursor = m.currentSection
			case "ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5":
				m.selectSectionShortcut(msg.String())
			case "ctrl+h":
				m.showHelp = true
			case "up", "k":
				m.moveLogSelection(-1)
			case "down", "j":
				m.moveLogSelection(1)
			case "pgup":
				pageSize := m.logBodyHeight()
				m.moveLogSelection(-pageSize)
			case "pgdown":
				pageSize := m.logBodyHeight()
				m.moveLogSelection(pageSize)
			case "G":
				m.logFollow = true
			case "enter":
				if len(m.filteredLogEntries()) > 0 {
					m.logDetailOpen = true
					m.logDetailScroll = 0
				}
			case "backspace":
				if r := []rune(m.logFilter); len(r) > 0 {
					m.logFilter = string(r[:len(r)-1])
					m.logScroll = 0
					m.logCursor = 0
				}
			default:
				s := msg.String()
				if len(s) == 1 && s[0] >= 32 && s[0] < 127 {
					m.logFilter += s
					m.logScroll = 0
					m.logCursor = 0
				}
			}
			return m, nil
		}

		switch msg.String() {
		case "/":
			m.sectionMenuOpen = true
			m.sectionMenuCursor = m.currentSection
		case "ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5":
			m.selectSectionShortcut(msg.String())
		case "ctrl+h":
			m.sectionMenuOpen = false
			m.showHelp = true
		case "ctrl+q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.sectionMenuOpen = false
			m.showHelp = false
		case "enter":
			if m.sectionMenuOpen {
				m.selectSection(m.sectionMenuCursor)
				break
			}
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
			if m.focusPanel == focusSessions {
				m.focusPanel = m.rightTabFocusPanel()
			} else if m.rightTab < 3 {
				m.rightTab++
				m.focusPanel = m.rightTabFocusPanel()
				m.rightScroll = 0
			} else {
				m.rightTab = 0
				m.focusPanel = focusSessions
			}
		case "shift+tab":
			if m.focusPanel == focusSessions {
				m.rightTab = 3
				m.focusPanel = m.rightTabFocusPanel()
			} else if m.rightTab > 0 {
				m.rightTab--
				m.focusPanel = m.rightTabFocusPanel()
				m.rightScroll = 0
			} else {
				m.focusPanel = focusSessions
			}
		case "up", "k":
			if m.sectionMenuOpen {
				if m.sectionMenuCursor > 0 {
					m.sectionMenuCursor--
				}
				break
			}
			switch m.focusPanel {
			case focusToolStats:
				if m.toolStatsCursor > 0 {
					m.toolStatsCursor--
				}
			case focusStats:
				if m.statsCursor > 0 {
					m.statsCursor--
				}
			case focusDetails, focusDiagnostics:
				if m.rightScroll > 0 {
					m.rightScroll--
				}
			default:
				if m.cursor > 0 {
					m.cursor--
					m.leftScroll = 0
					m.rightScroll = 0
					m.refreshStats()
				}
			}
		case "down", "j":
			if m.sectionMenuOpen {
				if m.sectionMenuCursor < len(sectionMenuItems)-1 {
					m.sectionMenuCursor++
				}
				break
			}
			switch m.focusPanel {
			case focusToolStats:
				if m.toolStatsCursor < len(m.toolStats)-1 {
					m.toolStatsCursor++
				}
			case focusStats:
				if m.statsCursor < len(m.recentCalls)-1 {
					m.statsCursor++
				}
			case focusDetails, focusDiagnostics:
				m.rightScroll++
			default:
				if m.cursor < len(m.sessions)-1 {
					m.cursor++
					m.leftScroll = 0
					m.rightScroll = 0
					m.refreshStats()
				}
			}
		case "1", "2", "3", "4", "5":
			if m.sectionMenuOpen {
				m.selectSection(int(msg.String()[0] - '1'))
				break
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
			if maxLeft := m.maxLeftWidth(); m.leftWidth > maxLeft {
				m.leftWidth = maxLeft
			}
		case "pgdown":
			pageSize := m.height - 6
			if pageSize < 1 {
				pageSize = 1
			}
			switch m.focusPanel {
			case focusToolStats:
				m.toolStatsCursor += pageSize
				if m.toolStatsCursor >= len(m.toolStats) {
					m.toolStatsCursor = len(m.toolStats) - 1
				}
			case focusStats:
				m.statsCursor += pageSize
				if m.statsCursor >= len(m.recentCalls) {
					m.statsCursor = len(m.recentCalls) - 1
				}
			case focusDetails, focusDiagnostics:
				m.rightScroll += pageSize
			default:
				m.cursor += pageSize
				if m.cursor >= len(m.sessions) {
					m.cursor = len(m.sessions) - 1
				}
				m.leftScroll = 0
				m.rightScroll = 0
				m.refreshStats()
			}
		case "pgup":
			pageSize := m.height - 6
			if pageSize < 1 {
				pageSize = 1
			}
			switch m.focusPanel {
			case focusToolStats:
				m.toolStatsCursor -= pageSize
				if m.toolStatsCursor < 0 {
					m.toolStatsCursor = 0
				}
			case focusStats:
				m.statsCursor -= pageSize
				if m.statsCursor < 0 {
					m.statsCursor = 0
				}
			case focusDetails, focusDiagnostics:
				m.rightScroll -= pageSize
				if m.rightScroll < 0 {
					m.rightScroll = 0
				}
			default:
				m.cursor -= pageSize
				if m.cursor < 0 {
					m.cursor = 0
				}
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

func (m Model) logBodyHeight() int {
	bodyHeight := m.height - 7
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	return bodyHeight
}

func (m Model) maxLeftWidth() int {
	maxLeft := m.width - 23
	if maxLeft < minLeftWidth {
		return minLeftWidth
	}
	return maxLeft
}

func (m Model) onDivider(x int) bool {
	return x == m.leftWidth+1
}

func (m Model) onSessionsPanel(x, y int) bool {
	if y < bodyStartRow || x <= 0 || x > m.leftWidth {
		return false
	}
	return len(m.sessions) > 0
}

func (m *Model) setLeftWidthFromMouse(x int) {
	next := x - 1
	if next < minLeftWidth {
		next = minLeftWidth
	}
	if maxLeft := m.maxLeftWidth(); next > maxLeft {
		next = maxLeft
	}
	m.leftWidth = next
}

func (m *Model) selectSessionAtBodyRow(row int) {
	if row < 1 {
		return
	}
	idx := (row - 1) / 3
	if idx < 0 || idx >= len(m.sessions) {
		return
	}
	m.cursor = idx
	m.focusPanel = focusSessions
	m.refreshStats()
}

func (m Model) onSectionSelector(x, y int) bool {
	return y >= 0 && y < 3 && x >= 0 && x < sectionMenuWidth
}

func (m *Model) selectSectionMenuAtRow(y int) {
	if y <= 0 || y > len(sectionMenuItems) {
		return
	}
	m.selectSection(y - 1)
}

func (m *Model) selectSectionShortcut(key string) {
	if !strings.HasPrefix(key, "ctrl+") || len(key) != len("ctrl+1") {
		return
	}
	idx := int(key[len(key)-1] - '1')
	if idx < 0 || idx >= len(sectionMenuItems) {
		return
	}
	m.selectSection(idx)
}

func (m *Model) selectSection(idx int) {
	if idx < 0 || idx >= len(sectionMenuItems) {
		return
	}
	m.currentSection = idx
	m.sectionMenuCursor = idx
	m.sectionMenuOpen = false
	if m.currentSection == 3 && !m.logInitd {
		m.logEntries, m.logOffset = initLogTail(m.logPath)
		m.logInitd = true
	}
}

func (m *Model) handleMouseWheel(mouse tea.Mouse, delta int) {
	if m.currentSection == 3 && !m.sectionMenuOpen && !m.showHelp {
		if m.logDetailOpen {
			m.logDetailScroll += delta
			if m.logDetailScroll < 0 {
				m.logDetailScroll = 0
			}
			return
		}
		m.moveLogSelection(delta)
		return
	}
	bodyHeight := m.height - 6
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	if mouse.Y < bodyStartRow || mouse.Y >= bodyStartRow+bodyHeight {
		return
	}
	if mouse.X <= m.leftWidth+1 {
		m.leftScroll += delta
		if m.leftScroll < 0 {
			m.leftScroll = 0
		}
		return
	}
	m.rightScroll += delta
	if m.rightScroll < 0 {
		m.rightScroll = 0
	}
}

func (m Model) render() string {
	// Logs section uses a dedicated full-width renderer.
	if m.currentSection == 3 && !m.showPopup {
		return m.renderLogsSection()
	}

	rightWidth := m.width - m.leftWidth - 3
	if rightWidth < 10 {
		rightWidth = 10
	}
	bodyHeight := m.height - 6
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	var sb strings.Builder
	isOverlay := m.showPopup || m.showHelp || m.sectionMenuOpen

	sepStyle := SepStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
	}

	// Header: 4-line Logo
	logoLines := strings.Split(LogoText, "\n")

	// Ensure logo starts at exactly right edge
	logoW := lipgloss.Width(logoLines[0])
	tabSpaceW := m.width - logoW

	menu := m.renderTopMenu(tabSpaceW, isOverlay)

	for i := 0; i < 3; i++ {
		// Draw the menu on the left, then the logo piece on the right.
		sb.WriteString(menu[i] + sepStyle.Render(logoLines[i]) + "\n")
	}
	sb.WriteString(m.renderTopBorder(rightWidth, isOverlay) + "\n")

	// Body content
	allLeftLines := m.leftLines()
	allRightLines := (&m).rightLines(rightWidth)

	// Clamp scroll offsets
	maxLeftScroll := len(allLeftLines) - bodyHeight
	if maxLeftScroll < 0 {
		maxLeftScroll = 0
	}
	if m.leftScroll > maxLeftScroll {
		m.leftScroll = maxLeftScroll
	}
	maxRightScroll := len(allRightLines) - bodyHeight
	if maxRightScroll < 0 {
		maxRightScroll = 0
	}
	if m.rightScroll > maxRightScroll {
		m.rightScroll = maxRightScroll
	}

	leftLines := allLeftLines[m.leftScroll:]
	rightLines := allRightLines[m.rightScroll:]

	leftScrollbar := scrollbarCol(len(allLeftLines), bodyHeight, m.leftScroll)
	rightScrollbar := scrollbarCol(len(allRightLines), bodyHeight, m.rightScroll)

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

		lBar := SepStyle.Render("│")
		if leftScrollbar != nil && i < len(leftScrollbar) {
			lBar = leftScrollbar[i]
		}
		rBar := SepStyle.Render("│")
		if rightScrollbar != nil && i < len(rightScrollbar) {
			rBar = rightScrollbar[i]
		}

		if isOverlay {
			lDim := InactiveStyle.Render(ansi.Strip(leftCell))
			rDim := InactiveStyle.Render(ansi.Strip(rightCell))
			line := sepStyle.Render("│") + lDim + sepStyle.Render("┆") + rDim + sepStyle.Render("│")
			sb.WriteString(line + "\n")
		} else {
			sb.WriteString(lBar + leftCell + SepStyle.Render("┆") + rightCell + rBar + "\n")
		}
	}

	sb.WriteString(m.renderBottomBorder(rightWidth, isOverlay) + "\n")
	sb.WriteString(m.renderMainStatusBar(isOverlay))

	final := sb.String()
	if m.showPopup {
		final = m.renderPopup(final, rightWidth, bodyHeight)
	}
	if m.showHelp {
		final = m.renderHelp(final)
	}
	if m.sectionMenuOpen {
		final = m.renderSectionMenuOverlay(final)
	}
	return final
}

func (m Model) renderMainStatusBar(dimmed bool) string {
	style := StatusStyle
	keyStyle := StatusKeyStyle
	if dimmed {
		style = InactiveStyle
		keyStyle = InactiveStyle
	}
	vStr := Version
	if vStr == "" {
		vStr = "dev"
	}
	sessCount := int64(len(m.sessions))
	memStr := "n/a"
	if m.daemonMetricsOK && m.daemonMetrics.RSSAvailable {
		memStr = monitor.FormatBytes(m.daemonMetrics.RSSBytes)
	}
	leftFooter := fmt.Sprintf(" plumb %s  ·  %s  ·  daemon mem: %s",
		vStr,
		formatSessionCount(sessCount),
		memStr,
	)
	rightFooterPlain := "/ menu  ·  ^q quit  ·  ^h help "
	footerGap := m.width - lipgloss.Width(leftFooter) - lipgloss.Width(rightFooterPlain)
	if footerGap < 1 {
		footerGap = 1
	}
	rightFooter := keyStyle.Render("/") + style.Render(" menu  ·  ") +
		keyStyle.Render("^q") + style.Render(" quit  ·  ") +
		keyStyle.Render("^h") + style.Render(" help ")
	return style.Render(leftFooter) + strings.Repeat(" ", footerGap) + rightFooter
}

func (m Model) renderTopBorder(rightWidth int, dimmed bool) string {
	var leftTitle string
	var leftStyle lipgloss.Style
	sepStyle := SepStyle

	if dimmed {
		sepStyle = SepInactiveStyle
		leftStyle = PanelHeaderInactiveStyle
	} else {
		if m.focusPanel == focusSessions {
			leftStyle = PanelHeaderStyle
		} else {
			leftStyle = PanelHeaderFadedStyle
		}
	}

	leftTitle = fmt.Sprintf(" Sessions (%d) ", len(m.sessions))

	leftPart := sepStyle.Render("╭─") + leftStyle.Render(leftTitle)
	leftFill := m.leftWidth - 1 - len(leftTitle)
	if leftFill < 0 {
		leftFill = 0
	}
	midPart := sepStyle.Render(strings.Repeat("─", leftFill) + "┬")

	logoBottom := strings.Split(LogoText, "\n")[3]
	currentW := lipgloss.Width(leftPart) + lipgloss.Width(midPart)
	fillerW := m.width - currentW - LogoWidth
	if fillerW < 0 {
		fillerW = 0
	}

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
	if m.popupLeftWidth == 0 {
		m.popupLeftWidth = minPopupLeftWidth
	}
	pLW, pRW := m.popupLeftWidth, m.width-m.popupLeftWidth-3
	if pRW < 10 {
		pRW = 10
	}

	var lines []string
	lines = append(lines, m.renderTopBorderPopup(pLW, pRW))
	allLeft := m.popupLeftLines()
	visibleLeft := allLeft[m.popupLeftScroll:]
	leftScrollbar := scrollbarCol(len(allLeft), bodyHeight, m.popupLeftScroll)
	allRight := m.popupRightAll(pRW - 2)
	maxDS := len(allRight) - bodyHeight
	if maxDS < 0 {
		maxDS = 0
	}
	if m.popupDetailScroll > maxDS {
		m.popupDetailScroll = maxDS
	}
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
		if i < len(visibleRight) {
			rStr = visibleRight[i]
		}
		rCell := lipgloss.NewStyle().Width(pRW).Render(rStr)

		lb := SepStyle.Render("│")
		if leftScrollbar != nil && i < len(leftScrollbar) {
			lb = leftScrollbar[i]
		}
		rb := SepStyle.Render("│")
		if scrollbar != nil && i < len(scrollbar) {
			rb = scrollbar[i]
		}

		lines = append(lines, lb+lCell+SepStyle.Render("┆")+rCell+rb)
	}
	lines = append(lines, m.renderBottomBorderPopup(pLW, pRW))
	return spliceOverlay(bg, strings.Join(lines, "\n"), m.width, m.height)
}

func (m Model) renderHelp(bg string) string {
	helpLines := []string{
		" ↑/↓ or j/k      Navigate sessions/calls/scroll details",
		" /               Open section selector",
		" ^1-^5           Open Dashboard/Sessions/Memory/Logs/Settings",
		" pgup/pgdown     Page through lists",
		" tab/shift+tab   Switch panel focus (sessions → details → tools → recent)",
		" enter           Open detail / select menu item",
		" [ and ]         Resize columns",
		" ^h              Open help",
		" ^q              Quit",
		" esc             Close popup/menu",
	}

	boxW := 76
	innerW := boxW - 2
	topLabel := " Help & Navigation "

	// Calculate dashes for the top border correctly
	labelW := lipgloss.Width(topLabel)
	leftDashes := 1
	rightDashes := innerW - labelW - leftDashes
	if rightDashes < 0 {
		rightDashes = 0
	}

	top := SepStyle.Render("╭─") + PanelHeaderStyle.Render(topLabel) + SepStyle.Render(strings.Repeat("─", rightDashes)+"╮")

	var bodyLines []string
	// Empty row top
	bodyLines = append(bodyLines, SepStyle.Render("│")+strings.Repeat(" ", innerW)+SepStyle.Render("│"))

	for _, l := range helpLines {
		content := "  " + padRight(l, innerW-4) + "  "
		bodyLines = append(bodyLines, SepStyle.Render("│")+content+SepStyle.Render("│"))
	}

	// Empty row bottom
	bodyLines = append(bodyLines, SepStyle.Render("│")+strings.Repeat(" ", innerW)+SepStyle.Render("│"))

	bottom := SepStyle.Render("╰" + strings.Repeat("─", innerW) + "╯")

	popup := top + "\n" + strings.Join(bodyLines, "\n") + "\n" + bottom
	return spliceOverlay(bg, popup, m.width, m.height)
}

func (m Model) renderSectionMenuOverlay(bg string) string {
	border := SepStyle
	textStyle := DetailStyle
	selectedStyle := SelectedStyle

	innerW := sectionMenuWidth - 2
	lines := []string{border.Render("╭" + strings.Repeat("─", innerW) + "╮")}
	for i, item := range sectionMenuItems {
		marker := " "
		style := textStyle
		if i == m.sectionMenuCursor {
			marker = "❯"
			style = selectedStyle
		}
		content := style.Render(fmt.Sprintf(" %s %d. %-11s  ", marker, i+1, item))
		pad := innerW - lipgloss.Width(content)
		if pad < 0 {
			pad = 0
		}
		lines = append(lines, border.Render("│")+content+strings.Repeat(" ", pad)+border.Render("│"))
	}
	lines = append(lines, border.Render("╰"+strings.Repeat("─", innerW)+"╯"))

	return spliceOverlayAt(bg, strings.Join(lines, "\n"), 0, 0)
}

func (m Model) renderTopBorderPopup(pLW, pRW int) string {
	lt, rt := " Timestamp ", " Call Detail "
	var lts, rts lipgloss.Style
	if m.popupRightFocus {
		lts, rts = PanelHeaderFadedStyle, PanelHeaderStyle
	} else {
		lts, rts = PanelHeaderStyle, PanelHeaderFadedStyle
	}
	lf := pLW - 1 - len(lt)
	if lf < 0 {
		lf = 0
	}
	rf := pRW - 1 - len(rt)
	if rf < 0 {
		rf = 0
	}
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
	if len(m.sessions) > 0 {
		currID = m.sessions[m.cursor].ID
	}
	for i, c := range m.popupCalls {
		sel := !m.popupRightFocus && i == m.popupCallCursor
		pre := "   "
		if i == m.popupCallCursor {
			pre = " > "
		}
		sc := "○"
		if c.SessionID == currID {
			sc = "●"
		}
		ok := "✓"
		if !c.Success {
			ok = "✗"
		}
		ts := c.CalledAt.Format("01-02 15:04:05")
		dur := fmt.Sprintf("%dms", c.DurationMs)
		row := fmt.Sprintf("%s%s %s %s %s", pre, sc, ok, ts, dur)
		maxW := m.popupLeftWidth - 1
		if maxW < 10 {
			maxW = 10
		}
		if lipgloss.Width(row) > maxW {
			row = string([]rune(row)[:maxW-1]) + "…"
		}
		if sel {
			lines = append(lines, SelectedStyle.Render(row))
		} else if m.popupRightFocus {
			lines = append(lines, TimestampFadedStyle.Render(row))
		} else if !c.Success {
			p1 := fmt.Sprintf("%s%s ", pre, sc)
			err := WarnStyle.Render("✗")
			p2 := fmt.Sprintf(" %s %s", ts, dur)
			lines = append(lines, TimestampActiveStyle.Render(p1)+err+TimestampActiveStyle.Render(p2))
		} else {
			lines = append(lines, TimestampActiveStyle.Render(row))
		}
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
	if len(m.sessions) > 0 {
		currID = m.sessions[m.cursor].ID
	}
	st := OkStyle.Render("✓ success")
	if !c.Success {
		st = WarnStyle.Render("✗ failed")
	}
	sl := MutedStyle.Render("○ historical")
	if c.SessionID == currID {
		sl = OkStyle.Render("● current")
	}
	sID := c.SessionID
	if len(sID) > 12 {
		sID = sID[:12] + "…"
	}
	sessLabel := sID + "  " + sl
	if c.SessionName != "" {
		sessLabel = DetailStyle.Render(c.SessionName) + "  " + sID + "  " + sl
	}
	lines = append(lines, detailRow("Tool", DetailStyle.Render(c.Tool)), detailRow("Status", st), detailRow("Called at", DetailStyle.Render(c.CalledAt.Format("2006-01-02 15:04:05"))), detailRow("Session", sessLabel), detailRow("Duration", DetailStyle.Render(fmt.Sprintf("%d ms", c.DurationMs))), detailRow("Input", DetailStyle.Render(fmt.Sprintf("%d bytes", c.InputBytes))), detailRow("Output", DetailStyle.Render(fmt.Sprintf("%d bytes", c.OutputBytes))))
	bx := func(label string, content []string) {
		inner := rw - 4
		if inner < 8 {
			inner = 8
		}
		tl := " " + label + " "
		tf := inner + 1 - len(tl)
		if tf < 0 {
			tf = 0
		}
		lines = append(lines, "", " "+SepStyle.Render("╭─")+PanelHeaderStyle.Render(tl)+SepStyle.Render(strings.Repeat("─", tf)+"╮"))
		for _, cl := range content {
			if lipgloss.Width(cl) > inner {
				cl = string([]rune(cl)[:inner-1]) + "…"
			}
			p := lipgloss.NewStyle().Width(inner).Render(cl)
			lines = append(lines, " "+SepStyle.Render("│")+" "+p+" "+SepStyle.Render("│"))
		}
		lines = append(lines, " "+SepStyle.Render("╰"+strings.Repeat("─", inner+2)+"╯"))
	}
	if !c.Success {
		var el []string
		if c.ErrorMsg != "" {
			for _, w := range wrapText(c.ErrorMsg, rw-5) {
				el = append(el, WarnStyle.Render(w))
			}
		} else {
			el = append(el, MutedStyle.Render("(no error message recorded)"))
		}
		bx("Error", el)
	}
	ij, ot := m.currentDetail()
	if ij != "" {
		var al []string
		var pb bytes.Buffer
		if err := json.Indent(&pb, []byte(ij), "", "  "); err == nil {
			for _, l := range strings.Split(strings.TrimRight(pb.String(), "\n"), "\n") {
				al = append(al, DetailStyle.Render(l))
			}
		} else {
			al = append(al, DetailStyle.Render(ij))
		}
		bx("Args", al)
	}
	if ot != "" && c.Success {
		var ol []string
		for _, o := range strings.Split(strings.TrimRight(ot, "\n"), "\n") {
			ol = append(ol, DetailStyle.Render(o))
		}
		bx("Output", ol)
	}
	if m.popupRightFocus {
		lines = append(lines, "", "  "+MutedStyle.Render("c copy · tab back"))
	}
	return lines
}

func scrollbarCol(total, visible, offset int) []string {
	if total <= visible {
		return nil
	}
	ts := visible * visible / total
	if ts < 1 {
		ts = 1
	}
	mo := total - visible
	if mo < 1 {
		mo = 1
	}
	tst := offset * (visible - ts) / mo
	col := make([]string, visible)
	for i := range visible {
		if i >= tst && i < tst+ts {
			col[i] = ScrollThumbStyle.Render("┃")
		} else {
			col[i] = ScrollTrackStyle.Render("│")
		}
	}
	return col
}

func (m Model) leftPanelHeader() string {
	bg := ActiveTheme.Border
	bgStyle := lipgloss.NewStyle().Background(bg)
	text := fmt.Sprintf("  Sessions (%d)", len(m.sessions))
	var textStyle lipgloss.Style
	if m.focusPanel == focusSessions {
		textStyle = bgStyle.Foreground(ActiveTheme.Accent).Bold(true)
	} else {
		textStyle = bgStyle.Foreground(ActiveTheme.TextFaded)
	}
	styled := textStyle.Render(text)
	remaining := m.leftWidth - lipgloss.Width(styled)
	if remaining > 0 {
		styled += bgStyle.Render(strings.Repeat(" ", remaining))
	}
	return styled
}

func (m Model) leftLines() []string {
	lf := m.focusPanel == focusSessions
	lines := []string{""}
	if len(m.sessions) == 0 {
		m1, m2 := " Daemon running.", " Call a tool to begin."
		if !daemonRunning() {
			m1, m2 = " No active sessions.", ""
		}
		if lf {
			lines = append(lines, MutedStyle.Render(m1))
			if m2 != "" {
				lines = append(lines, MutedStyle.Render(m2))
			}
		} else {
			lines = append(lines, InactiveStyle.Render(m1))
			if m2 != "" {
				lines = append(lines, InactiveStyle.Render(m2))
			}
		}
		return lines
	}
	for i, s := range m.sessions {
		selected := i == m.cursor
		indicator := "○"
		if selected {
			indicator = "❯"
		}
		name := s.Name
		if name == "" {
			name = s.ID
		}
		firstLine := " " + indicator + " " + name
		if s.Language != "" && s.Language != "none" {
			firstLine += " " + sessionLangBadge(s.Language, selected, lf)
		}
		if s.Synthetic {
			firstLine += " (auto)"
		}
		path := "resolving…"
		if s.Folder != "" {
			mf := m.leftWidth - len([]rune("    ╰─ "))
			if mf < 0 {
				mf = 0
			}
			path = contractPath(s.Folder, mf)
		}
		secondLine := "    ╰─ " + path
		if i == m.cursor {
			if lf {
				lines = append(lines, SelectedStyle.Render(firstLine))
				lines = append(lines, SelectedStyle.Render(secondLine))
			} else {
				lines = append(lines, FadedStyle.Render(firstLine))
				lines = append(lines, FadedStyle.Render(secondLine))
			}
		} else {
			if lf {
				lines = append(lines, ItemStyle.Render(firstLine))
				lines = append(lines, MutedStyle.Render(secondLine))
			} else {
				lines = append(lines, FadedStyle.Render(firstLine))
				lines = append(lines, FadedStyle.Render(secondLine))
			}
		}
		lines = append(lines, "")
	}
	return lines
}

func sessionLangBadge(language string, selected, focused bool) string {
	badge := " " + language + " "
	switch {
	case selected && focused:
		return SessionLangSelectedStyle.Render(badge)
	case focused:
		return SessionLangStyle.Render(badge)
	default:
		return SessionLangFadedStyle.Render(badge)
	}
}

func (m *Model) handleRightPanelClick(bodyRow int) {
	switch m.rightTab {
	case 1: // Tools tab
		if m.statsTableBodyRow >= 0 && len(m.toolStats) > 0 {
			idx := bodyRow - m.statsTableBodyRow
			if idx >= 0 && idx < len(m.toolStats) {
				m.toolStatsCursor, m.focusPanel = idx, focusToolStats
			}
		}
	case 2: // History tab
		if m.recentTableBodyRow >= 0 && len(m.recentCalls) > 0 {
			idx := bodyRow - m.recentTableBodyRow
			if idx >= 0 && idx < len(m.recentCalls) {
				m.statsCursor, m.focusPanel = idx, focusStats
			}
		}
	}
}

// rightTabFocusPanel returns the panelFocus that corresponds to the current rightTab.
func (m Model) rightTabFocusPanel() panelFocus {
	switch m.rightTab {
	case 1:
		return focusToolStats
	case 2:
		return focusStats
	case 3:
		return focusDiagnostics
	default:
		return focusDetails
	}
}

// rightTabBar renders the pill-style tab header row for the right panel.
// Active pill uses accent colour; inactive pills use muted text.
func (m Model) rightTabBar(_ int) string {
	activeLabel := lipgloss.NewStyle().Foreground(ActiveTheme.Accent).Bold(true)
	activeBracket := lipgloss.NewStyle().Foreground(ActiveTheme.Accent)
	inactiveLabel := lipgloss.NewStyle().Foreground(ActiveTheme.TextFaded)
	inactiveBracket := lipgloss.NewStyle().Foreground(ActiveTheme.TextMuted)
	rightFocused := m.focusPanel != focusSessions

	tabs := []string{"Details", "Tools", "History", "Diagnostics"}
	var sb strings.Builder
	sb.WriteString(" ")
	for i, name := range tabs {
		if i > 0 {
			sb.WriteString("  ")
		}
		if i == m.rightTab && rightFocused {
			sb.WriteString(activeBracket.Render("[") + activeLabel.Render(" "+name+" ") + activeBracket.Render("]"))
		} else {
			sb.WriteString(inactiveBracket.Render("[") + inactiveLabel.Render(" "+name+" ") + inactiveBracket.Render("]"))
		}
	}
	return sb.String()
}

func (m *Model) rightLines(rw int) []string {
	lines := []string{m.rightTabBar(rw), ""}
	if len(m.sessions) == 0 {
		lines = append(lines, "  "+MutedStyle.Render("Select a session to view details."))
		return lines
	}
	switch m.rightTab {
	case 1:
		lines = append(lines, m.rightLinesTools(rw)...)
	case 2:
		lines = append(lines, m.rightLinesHistory(rw)...)
	case 3:
		lines = append(lines, m.rightLinesDiagnostics(rw)...)
	default:
		lines = append(lines, m.rightLinesDetails(rw)...)
	}
	return lines
}

func (m Model) rightLinesDetails(rw int) []string {
	const kw = 14
	mv := rw - kw
	if mv < 8 {
		mv = 8
	}
	s := m.sessions[m.cursor]
	fld := s.Folder
	if fld == "" {
		fld = MutedStyle.Render("(resolving workspace…)")
	} else {
		fld = contractPath(fld, mv)
	}
	adp := s.Adapter
	if adp == "" {
		adp = "—"
	}
	nm := s.Name
	if nm == "" {
		nm = MutedStyle.Render("—")
	}
	lines := []string{
		detailRow("Name", nm),
		detailRow("ID", s.ID),
		detailRow("Language", s.Language),
		detailRow("Folder", fld),
		detailRow("Adapter", adp),
		detailRow("PID", fmt.Sprintf("%d", s.PID)),
	}
	if s.DaemonVersion != "" {
		lines = append(lines, detailRow("Daemon", s.DaemonVersion))
	}
	lines = append(lines, detailRow("Started", s.StartedAt.Format("2006-01-02 15:04:05")))
	cl := s.ClientName
	if s.ClientVersion != "" {
		cl += " " + s.ClientVersion
	}
	if cl == "" {
		cl = MutedStyle.Render("unknown")
	}
	lines = append(lines, detailRow("Client", cl))
	return lines
}

func (m *Model) rightLinesTools(rw int) []string {
	const (
		c2w, c3w, c4w = 8, 10, 6
	)
	s3 := "   "
	c1w := rw - 2 - c2w - c3w - c4w - 12
	if c1w < 10 {
		c1w = 10
	}
	sln := "  " + SepStyle.Render(strings.Repeat("─", rw-3))
	roww := rw - 2

	if len(m.toolStats) == 0 {
		m.statsTableBodyRow = -1
		return []string{
			"  " + MutedStyle.Render("No calls recorded yet."),
		}
	}
	lc := padRight(HintStyle.Render("Tool"), c1w)
	h := "  " + lc + s3 + padLeft(HintStyle.Render("Calls"), c2w) + s3 + padLeft(HintStyle.Render("Avg"), c3w) + s3 + HintStyle.Render("Errors")
	lines := []string{h, sln}
	m.statsTableBodyRow = 2 // tab bar + blank = 2 rows before this content
	for i, ts := range m.toolStats {
		sel := m.focusPanel == focusToolStats && i == m.toolStatsCursor
		tn := padRight(truncate(ts.Tool, c1w-2), c1w-2)
		if sel {
			pc, pa, pe := padLeft(fmt.Sprintf("%d", ts.Calls), c2w), padLeft(fmt.Sprintf("%.0fms", ts.AvgMs), c3w), padLeft("", c4w)
			if ts.Errors > 0 {
				pe = padLeft(fmt.Sprintf("%d", ts.Errors), c4w)
			}
			lines = append(lines, SelectedStyle.Width(roww).Render("  > "+tn+s3+pc+s3+pa+s3+pe+s3))
		} else {
			c2, c3, c4 := padLeft(OkStyle.Render(fmt.Sprintf("%d", ts.Calls)), c2w), padLeft(MutedStyle.Render(fmt.Sprintf("%.0fms", ts.AvgMs)), c3w), padLeft("", c4w)
			if ts.Errors > 0 {
				c4 = padLeft(WarnStyle.Render(fmt.Sprintf("%d", ts.Errors)), c4w)
			}
			lines = append(lines, "  ○ "+tn+s3+c2+s3+c3+s3+c4+s3)
		}
	}
	return lines
}

func (m *Model) rightLinesHistory(rw int) []string {
	const (
		c2w, c3w, c4w, c5w = 8, 10, 6, 12
	)
	s3 := "   "
	c1w := rw - 2 - c2w - c3w - c4w - 12
	if c1w < 10 {
		c1w = 10
	}
	rc1w := c1w - c5w - 3
	if rc1w < 8 {
		rc1w = 8
	}
	sln := "  " + SepStyle.Render(strings.Repeat("─", rw-3))
	roww := rw - 2

	if len(m.recentCalls) == 0 {
		m.recentTableBodyRow = -1
		return []string{
			"  " + MutedStyle.Render("No calls in this session yet."),
		}
	}
	rlc := padRight(HintStyle.Render("Tool"), rc1w)
	h := "  " + rlc + s3 + padLeft(HintStyle.Render("Dur"), c2w) + s3 + padLeft(HintStyle.Render("When"), c3w) + s3 + padLeft(HintStyle.Render("Err"), c4w) + s3 + HintStyle.Render("Session")
	lines := []string{h, sln}
	m.recentTableBodyRow = 2 // tab bar + blank = 2 rows before this content
	for i, c := range m.recentCalls {
		sel := m.focusPanel == focusStats && i == m.statsCursor
		tn := padRight(truncate(c.Tool, rc1w-2), rc1w-2)
		sn := padRight(truncate(c.SessionName, c5w), c5w)
		if sel {
			pd, pw, pe := padLeft(fmt.Sprintf("%dms", c.DurationMs), c2w), padLeft(humanAgeTUI(c.CalledAt), c3w), padLeft("", c4w)
			if !c.Success {
				pe = padLeft("✗", c4w)
			}
			lines = append(lines, SelectedStyle.Width(roww).Render("  > "+tn+s3+pd+s3+pw+s3+pe+s3+sn))
		} else {
			mk := OkStyle.Render("✓") + " "
			if !c.Success {
				mk = WarnStyle.Render("✗") + " "
			}
			c2, c3, c4 := padLeft(MutedStyle.Render(fmt.Sprintf("%dms", c.DurationMs)), c2w), padLeft(MutedStyle.Render(humanAgeTUI(c.CalledAt)), c3w), padLeft("", c4w)
			if !c.Success {
				c4 = padLeft(WarnStyle.Render("✗"), c4w)
			}
			c5 := padRight(MutedStyle.Render(truncate(c.SessionName, c5w)), c5w)
			lines = append(lines, "  "+mk+tn+s3+c2+s3+c3+s3+c4+s3+c5)
		}
	}
	return lines
}

func (m Model) rightLinesDiagnostics(_ int) []string {
	if m.lastDiagnosticsOutput == "" {
		return []string{
			"",
			"  " + MutedStyle.Render("No diagnostics recorded yet."),
			"  " + MutedStyle.Render("Run the `diagnostics` tool in this session to populate this tab."),
		}
	}
	var lines []string
	lines = append(lines, "")
	for _, line := range strings.Split(m.lastDiagnosticsOutput, "\n") {
		if line == "" {
			lines = append(lines, "")
		} else {
			lines = append(lines, "  "+DetailStyle.Render(line))
		}
	}
	return lines
}

func padRight(s string, w int) string {
	v := lipgloss.Width(s)
	if v >= w {
		return s
	}
	return s + strings.Repeat(" ", w-v)
}

func (m Model) renderTopMenu(width int, dimmed bool) []string {
	selector := m.renderSectionSelector(dimmed)
	activityBox := m.renderActivityBox(dimmed)
	daemonBox := m.renderDaemonMetricsBox(dimmed)
	tokenBox := m.renderTokenSavingsBox(dimmed)
	selectorWidth := lipgloss.Width(selector[0])
	daemonBoxWidth := lipgloss.Width(daemonBox[0])
	showDaemonBox := width >= selectorWidth+1+daemonBoxWidth+1+30
	currentWidth := selectorWidth + 1 + 30
	if showDaemonBox {
		currentWidth += 1 + daemonBoxWidth
	}
	showTokenBox := width >= currentWidth+1+lipgloss.Width(tokenBox[0])
	out := make([]string, 0, len(selector))
	for i := range selector {
		line := selector[i]
		if showDaemonBox {
			line += " " + daemonBox[i]
		}
		line += " " + activityBox[i]
		if showTokenBox {
			line += " " + tokenBox[i]
		}
		pad := width - lipgloss.Width(line)
		if pad < 0 {
			pad = 0
		}
		out = append(out, line+strings.Repeat(" ", pad))
	}
	return out
}

func (m Model) renderDaemonMetricsBox(dimmed bool) []string {
	border := SepStyle
	title := PanelHeaderFadedStyle
	sparkStyle := SelectedStyle
	if dimmed {
		border = SepInactiveStyle
		title = PanelHeaderInactiveStyle
		sparkStyle = InactiveStyle
	}

	const (
		boxWidth   = 24
		innerWidth = boxWidth - 2
		sparkWidth = innerWidth - 2
	)

	value := "n/a"
	if m.daemonMetricsOK && m.daemonMetrics.CPUAvailable {
		value = monitor.FormatCPU(m.daemonMetrics.CPUPercent)
	}
	titleText := " Daemon CPU (" + value + ") "
	topFill := boxWidth - lipgloss.Width("╭─") - lipgloss.Width(titleText) - lipgloss.Width("╮")
	if topFill < 0 {
		topFill = 0
	}

	spark := cpuSparkline(m.daemonCPU, sparkWidth)
	content := " " + sparkStyle.Render(spark) + " "

	return []string{
		border.Render("╭─") + title.Render(titleText) + border.Render(strings.Repeat("─", topFill)+"╮"),
		border.Render("│") + content + border.Render("│"),
		border.Render("╰" + strings.Repeat("─", innerWidth) + "╯"),
	}
}

func (m Model) renderSectionSelector(dimmed bool) []string {
	border := SepStyle
	textStyle := SelectedStyle
	hintStyle := MutedStyle
	if dimmed {
		border = SepInactiveStyle
		textStyle = InactiveStyle
		hintStyle = InactiveStyle
	}

	current := "Sessions"
	if m.currentSection >= 0 && m.currentSection < len(sectionMenuItems) {
		current = sectionMenuItems[m.currentSection]
	}
	sectionNum := 1
	if m.currentSection >= 0 && m.currentSection < len(sectionMenuItems) {
		sectionNum = m.currentSection + 1
	}
	content := fmt.Sprintf(" %s ", textStyle.Render(fmt.Sprintf("❯ %d. %s", sectionNum, current)))
	arrow := hintStyle.Render("▽")
	pad := sectionMenuWidth - 2 - lipgloss.Width(content) - lipgloss.Width(arrow) - 1
	if pad < 1 {
		pad = 1
	}
	row := content + strings.Repeat(" ", pad) + arrow + " "

	return []string{
		border.Render("╭" + strings.Repeat("─", sectionMenuWidth-2) + "╮"),
		border.Render("│") + row + border.Render("│"),
		border.Render("╰" + strings.Repeat("─", sectionMenuWidth-2) + "╯"),
	}
}

func (m Model) renderTokenSavingsBox(dimmed bool) []string {
	border := SepStyle
	title := PanelHeaderFadedStyle
	barStyle := SelectedStyle
	valueStyle := DetailStyle
	if dimmed {
		border = SepInactiveStyle
		title = PanelHeaderInactiveStyle
		barStyle = InactiveStyle
		valueStyle = InactiveStyle
	}

	const (
		barWidth = 16
	)

	titleText := " Tokens Saved "
	value := stats.FormatSavings(int(m.tokenSavings))
	filledPart, unfilledPart := tokenSavingsBar(m.tokenSavings, barWidth)
	content := " " + barStyle.Render(filledPart) + border.Render(unfilledPart) + " " + valueStyle.Render(value) + " "
	innerWidth := lipgloss.Width(content)
	minInnerWidth := lipgloss.Width("─") + lipgloss.Width(titleText)
	if innerWidth < minInnerWidth {
		innerWidth = minInnerWidth
	}
	topFill := innerWidth - lipgloss.Width("─") - lipgloss.Width(titleText)
	if topFill < 0 {
		topFill = 0
	}

	return []string{
		border.Render("╭─") + title.Render(titleText) + border.Render(strings.Repeat("─", topFill)+"╮"),
		border.Render("│") + content + border.Render("│"),
		border.Render("╰" + strings.Repeat("─", innerWidth) + "╯"),
	}
}

func formatUptime(d time.Duration) string {
	if d < time.Minute {
		return "< 1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm+", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh+", int(d.Hours()))
	}
	if d < 30*24*time.Hour {
		return fmt.Sprintf("%dd+", int(d.Hours()/24))
	}
	if d < 365*24*time.Hour {
		return fmt.Sprintf("%dmo+", int(d.Hours()/(24*30)))
	}
	return fmt.Sprintf("%dy+", int(d.Hours()/(24*365)))
}

func (m Model) renderActivityBox(dimmed bool) []string {
	border := SepStyle
	title := PanelHeaderFadedStyle
	sparkStyle := SelectedStyle
	countStyle := DetailStyle
	if dimmed {
		border = SepInactiveStyle
		title = PanelHeaderInactiveStyle
		sparkStyle = InactiveStyle
		countStyle = InactiveStyle
	}

	const (
		boxWidth      = 30
		innerWidth    = boxWidth - 2
		maxSparkWidth = 16
	)

	windowStr := "1m"
	if m.activity.Window > 0 {
		windowStr = formatUptime(m.activity.Window)
	}
	titleText := fmt.Sprintf(" Activity (%s) ", windowStr)

	topFill := boxWidth - lipgloss.Width("╭─") - lipgloss.Width(titleText) - lipgloss.Width("╮")
	if topFill < 0 {
		topFill = 0
	}

	count := formatActivityCalls(m.activity.Calls)
	countWidth := lipgloss.Width(count)
	sparkWidth := innerWidth - countWidth - 3 // left pad + gap + right pad
	if sparkWidth > maxSparkWidth {
		sparkWidth = maxSparkWidth
	}
	if sparkWidth < 1 {
		sparkWidth = 1
	}
	spark := activitySparkline(m.activity.Buckets, sparkWidth)
	middlePad := innerWidth - lipgloss.Width(spark) - countWidth - 2
	if middlePad < 1 {
		middlePad = 1
	}
	content := " " + sparkStyle.Render(spark) + strings.Repeat(" ", middlePad) + countStyle.Render(count) + " "

	return []string{
		border.Render("╭─") + title.Render(titleText) + border.Render(strings.Repeat("─", topFill)+"╮"),
		border.Render("│") + content + border.Render("│"),
		border.Render("╰" + strings.Repeat("─", innerWidth) + "╯"),
	}
}

func activitySparkline(buckets []int64, width int) string {
	if width <= 0 {
		return ""
	}
	if len(buckets) == 0 {
		return strings.Repeat(" ", width)
	}
	out := make([]rune, width)
	var max int64 = 10 // Enforce a minimum ceiling so 1-2 calls don't draw a full 100% bar
	for _, v := range buckets {
		if v > max {
			max = v
		}
	}
	levels := []rune("⡀⡄⡆⡇⣇⣧⣷⣿")
	for i := range width {
		bucketIdx := i * len(buckets) / width
		v := buckets[bucketIdx]
		if max == 0 || v == 0 {
			out[i] = ' '
			continue
		}
		levelIdx := int((v*int64(len(levels)) - 1) / max)
		if levelIdx < 0 {
			levelIdx = 0
		}
		if levelIdx >= len(levels) {
			levelIdx = len(levels) - 1
		}
		out[i] = levels[levelIdx]
	}
	return string(out)
}

func cpuSparkline(samples []float64, width int) string {
	if width <= 0 {
		return ""
	}
	if len(samples) == 0 {
		return strings.Repeat(" ", width)
	}
	out := make([]rune, width)
	levels := []rune("⡀⡄⡆⡇⣇⣧⣷⣿")
	for i := range width {
		sampleIdx := i * len(samples) / width
		v := clampPercent(samples[sampleIdx])
		if v == 0 {
			out[i] = ' '
			continue
		}
		levelIdx := int((v*float64(len(levels)) - 0.001) / 100)
		if levelIdx < 0 {
			levelIdx = 0
		}
		if levelIdx >= len(levels) {
			levelIdx = len(levels) - 1
		}
		out[i] = levels[levelIdx]
	}
	return string(out)
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func tokenSavingsBar(tokens int64, width int) (string, string) {
	if width <= 0 {
		return "", ""
	}
	const targetTokens = 1_500_000
	filled := int(tokens * int64(width) / targetTokens)
	if tokens > 0 && filled == 0 {
		filled = 1
	}
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("█", filled), strings.Repeat("░", width-filled)
}

func formatActivityCalls(n int64) string {
	switch {
	case n >= 1_000_000:
		val := float64(n) / 1_000_000
		if val == float64(int64(val)) {
			return fmt.Sprintf("%.0fm calls", val)
		}
		return fmt.Sprintf("%.1fm calls", val)
	case n >= 1000:
		val := float64(n) / 1000
		if val >= 100 || val == float64(int64(val)) {
			return fmt.Sprintf("%.0fk calls", val)
		}
		return fmt.Sprintf("%.1fk calls", val)
	case n == 1:
		return "1 call"
	default:
		return fmt.Sprintf("%d calls", n)
	}
}

func formatSessionCount(n int64) string {
	switch n {
	case 0:
		return "no sessions"
	case 1:
		return "1 session"
	default:
		return fmt.Sprintf("%d sessions", n)
	}
}

func formatToolCallCount(n int64) string {
	return fmt.Sprintf("%d %s", n, pluralWord(n, "tool call", "tool calls"))
}

func pluralWord(n int64, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

func padLeft(s string, w int) string {
	v := lipgloss.Width(s)
	if v >= w {
		return s
	}
	return strings.Repeat(" ", w-v) + s
}

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
	return append(lines, cur)
}

func detailRow(k, v string) string { return "  " + KeyStyle.Render(k) + ValStyle.Render(v) }

func contractPath(p string, max int) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		p = "~" + p[len(h):]
	}
	r := []rune(p)
	if len(r) <= max {
		return p
	}
	if max <= 1 {
		return "…"
	}
	return "…" + string(r[len(r)-(max-1):])
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

func daemonRunning() bool {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	_, err = os.Stat(filepath.Join(base, "plumb", "plumb.sock"))
	return err == nil
}

func copyToClipboard(c stats.RecentCall, ij, ot string) tea.Cmd {
	return copyTextToClipboard(formatCallDetailForClipboard(ij, ot))
}

func formatCallDetailForClipboard(ij, ot string) string {
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
	if ot != "" {
		buf.WriteString("=== Output ===\n")
		buf.WriteString(ot)
		buf.WriteString("\n")
	}
	return buf.String()
}

func copyTextToClipboard(txt string) tea.Cmd {
	return func() tea.Msg {
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
			cmd.Stdin = strings.NewReader(txt)
			_ = cmd.Run()
		}
		return nil
	}
}

func spliceOverlay(bg, overlay string, w, h int) string {
	ovLines := strings.Split(overlay, "\n")
	ovH := len(ovLines)
	ovW := 0
	for _, l := range ovLines {
		if lw := lipgloss.Width(l); lw > ovW {
			ovW = lw
		}
	}
	sy, sx := (h-ovH)/2, (w-ovW)/2
	return spliceOverlayAt(bg, overlay, sx, sy)
}

func spliceOverlayLower(bg, overlay string, w, h int) string {
	ovLines := strings.Split(overlay, "\n")
	ovH := len(ovLines)
	ovW := 0
	for _, l := range ovLines {
		if lw := lipgloss.Width(l); lw > ovW {
			ovW = lw
		}
	}
	sy, sx := (h-ovH)/2+1, (w-ovW)/2
	return spliceOverlayAt(bg, overlay, sx, sy)
}

func dimAll(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = InactiveStyle.Render(ansi.Strip(line))
	}
	return strings.Join(lines, "\n")
}

func spliceOverlayAt(bg, overlay string, sx, sy int) string {
	bgLines := strings.Split(bg, "\n")
	ovLines := strings.Split(overlay, "\n")
	ovW := 0
	for _, l := range ovLines {
		if lw := lipgloss.Width(l); lw > ovW {
			ovW = lw
		}
	}
	for i := 0; i < len(ovLines); i++ {
		y := sy + i
		if y < 0 || y >= len(bgLines) {
			continue
		}
		bl := bgLines[y]
		ol := ovLines[i]

		// Ensure overlay line is full width
		currOW := lipgloss.Width(ol)
		if currOW < ovW {
			ol += strings.Repeat(" ", ovW-currOW)
		}

		prefix := ansi.Truncate(bl, sx, "")
		suffix := ansi.TruncateLeft(bl, sx+ovW, "")

		bgLines[y] = InactiveStyle.Render(ansi.Strip(prefix)) + ol + InactiveStyle.Render(ansi.Strip(suffix))
	}
	return strings.Join(bgLines, "\n")
}

func Run(logPath string) error {
	RebuildStyles()
	p := tea.NewProgram(NewModel(logPath))
	_, err := p.Run()
	return err
}

// filteredLogEntries returns log entries that match the current filter string
// (case-insensitive substring match on the raw line). Returns all entries when
// the filter is empty.
func (m Model) filteredLogEntries() []logEntry {
	if m.logFilter == "" {
		return m.logEntries
	}
	lower := strings.ToLower(m.logFilter)
	var out []logEntry
	for _, e := range m.logEntries {
		if strings.Contains(strings.ToLower(e.Raw), lower) {
			out = append(out, e)
		}
	}
	return out
}

// renderLogEntry formats a single log entry for display within width visible
// characters. Structured JSON entries are rendered with a timestamp, level
// badge, message, and key=val attributes; plain-text entries are shown as-is.
func (m Model) renderLogEntry(e logEntry, width int, selected bool) string {
	prefixMark := "  "
	if selected {
		prefixMark = " ❯"
	}
	if e.Msg == "" {
		// Plain text line — just show raw content.
		return prefixMark + " " + MutedStyle.Render(truncate(e.Raw, width-3))
	}

	// Timestamp: "15:04:05" (8 chars) or blank.
	ts := "        "
	if !e.Time.IsZero() {
		ts = e.Time.Format("15:04:05")
	}

	// Level badge padded to 5 chars.
	const levelW = 5
	lvlText := padRight(e.Level, levelW)
	var lvlStyled string
	switch strings.ToUpper(strings.TrimSpace(e.Level)) {
	case "ERROR":
		lvlStyled = WarnStyle.Render(lvlText)
	case "WARN", "WARNING":
		lvlStyled = WarnStyle.Render(lvlText)
	case "DEBUG":
		lvlStyled = MutedStyle.Render(lvlText)
	default: // INFO and unknown
		lvlStyled = OkStyle.Render(lvlText)
	}

	// Attrs: key=val pairs, sorted for deterministic output.
	var attrParts []string
	if len(e.Attrs) > 0 {
		keys := make([]string, 0, len(e.Attrs))
		for k := range e.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			attrParts = append(attrParts, k+"="+e.Attrs[k])
		}
	}

	prefix := prefixMark + " " + MutedStyle.Render(ts) + " " + lvlStyled + "  "
	msg := DetailStyle.Render(e.Msg)
	attrs := ""
	if len(attrParts) > 0 {
		attrs = "  " + MutedStyle.Render(strings.Join(attrParts, " "))
	}

	line := prefix + msg + attrs
	// ANSI-aware truncation to keep within the cell boundary.
	if lipgloss.Width(line) > width-1 {
		line = ansi.Truncate(line, width-2, "…")
	}
	return line
}

// renderTopBorderLogs builds the plain top border for the Logs section.
func (m Model) renderTopBorderLogs(dimmed bool) string {
	sep := SepStyle
	if dimmed {
		sep = SepInactiveStyle
	}
	logoBottom := strings.Split(LogoText, "\n")[3]
	fill := m.width - LogoWidth - 1
	if fill < 0 {
		fill = 0
	}
	return sep.Render("╭"+strings.Repeat("─", fill)) + sep.Render(logoBottom)
}

// logBodyScroll computes the clamped scroll offset for the log body given the
// total number of filtered entries and the available body height.
func (m Model) logBodyScroll(total, bodyHeight int) int {
	maxScroll := total - bodyHeight
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.logFollow {
		return maxScroll
	}
	s := m.logScroll
	if s > maxScroll {
		return maxScroll
	}
	if s < 0 {
		return 0
	}
	return s
}

func (m Model) selectedLogIndex(filteredLen int) int {
	if filteredLen == 0 {
		return 0
	}
	if m.logFollow {
		return filteredLen - 1
	}
	if m.logCursor < 0 {
		return 0
	}
	if m.logCursor >= filteredLen {
		return filteredLen - 1
	}
	return m.logCursor
}

func (m *Model) moveLogSelection(delta int) {
	filtered := m.filteredLogEntries()
	if len(filtered) == 0 {
		m.logCursor = 0
		m.logScroll = 0
		m.logFollow = false
		return
	}
	m.logCursor = m.selectedLogIndex(len(filtered)) + delta
	if m.logCursor < 0 {
		m.logCursor = 0
	}
	if m.logCursor >= len(filtered) {
		m.logCursor = len(filtered) - 1
	}
	m.logFollow = false
	m.ensureLogCursorVisible(len(filtered))
}

func (m *Model) ensureLogCursorVisible(total int) {
	bodyHeight := m.logBodyHeight()
	maxScroll := total - bodyHeight
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.logCursor < m.logScroll {
		m.logScroll = m.logCursor
	}
	if m.logCursor >= m.logScroll+bodyHeight {
		m.logScroll = m.logCursor - bodyHeight + 1
	}
	if m.logScroll < 0 {
		m.logScroll = 0
	}
	if m.logScroll > maxScroll {
		m.logScroll = maxScroll
	}
}

func (m *Model) selectLogAtBodyRow(row int) {
	bodyHeight := m.logBodyHeight()
	if row < 0 || row >= bodyHeight {
		return
	}
	filtered := m.filteredLogEntries()
	if len(filtered) == 0 {
		return
	}
	scroll := m.logBodyScroll(len(filtered), bodyHeight)
	idx := scroll + row
	if idx >= len(filtered) {
		return
	}
	m.logCursor = idx
	m.logScroll = scroll
	m.logFollow = false
}

// renderLogBodyLine renders one row of the log body, applying the isOverlay
// dim treatment when an overlay panel is open.
func (m Model) renderLogBodyLine(entry *logEntry, innerW int, selected bool, isOverlay bool, rBar string) string {
	var line string
	if entry != nil {
		line = m.renderLogEntry(*entry, innerW-2, selected)
	}
	if isOverlay {
		cell := lipgloss.NewStyle().Width(innerW - 2).Render(line)
		return SepInactiveStyle.Render("│") + " " + InactiveStyle.Render(ansi.Strip(cell)) + " " + rBar
	}
	cell := lipgloss.NewStyle().Width(innerW - 2).Render(line)
	if selected && entry != nil {
		cell = LogSelectedStyle.Width(innerW - 2).Render(line)
	}
	return SepStyle.Render("│") + " " + cell + " " + rBar
}

// renderLogsSection renders the full terminal content for the Logs section.
// It reuses the standard top menu and logo header but replaces the two-panel
// body with a full-width, scrollable log viewer.
func (m Model) renderLogsSection() string {
	bodyHeight := m.logBodyHeight()
	innerW := m.width - 2 // visible content width inside │ borders

	var sb strings.Builder
	isOverlay := m.showHelp || m.sectionMenuOpen

	sepStyle := SepStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
	}

	// Header: 3-line top menu + logo.
	logoLines := strings.Split(LogoText, "\n")
	logoW := lipgloss.Width(logoLines[0])
	menu := m.renderTopMenu(m.width-logoW, isOverlay)
	for i := range 3 {
		sb.WriteString(menu[i] + sepStyle.Render(logoLines[i]) + "\n")
	}
	sb.WriteString(m.renderTopBorderLogs(isOverlay) + "\n")

	// Body: filtered log entries with scroll.
	filtered := m.filteredLogEntries()
	scroll := m.logBodyScroll(len(filtered), bodyHeight)
	visible := filtered[scroll:]
	scrollbar := scrollbarCol(len(filtered), bodyHeight, scroll)
	selectedIdx := m.selectedLogIndex(len(filtered))

	for i := range bodyHeight {
		rBar := SepStyle.Render("│")
		if scrollbar != nil && i < len(scrollbar) {
			rBar = scrollbar[i]
		}
		var entry *logEntry
		if i < len(visible) {
			e := visible[i]
			entry = &e
		}
		sb.WriteString(m.renderLogBodyLine(entry, innerW, scroll+i == selectedIdx, isOverlay, rBar) + "\n")
	}

	// In-frame status bar and bottom border.
	sb.WriteString(m.renderLogStatusBar(filtered, innerW, isOverlay) + "\n")
	sb.WriteString(sepStyle.Render("╰"+strings.Repeat("─", innerW)+"╯") + "\n")
	sb.WriteString(m.renderMainStatusBar(isOverlay))

	final := sb.String()
	if m.logDetailOpen {
		final = m.renderLogDetail(final, filtered)
	}
	if m.showHelp {
		final = m.renderHelp(final)
	}
	if m.sectionMenuOpen {
		final = m.renderSectionMenuOverlay(final)
	}
	return final
}

// renderLogStatusBar builds the in-frame status bar for the Logs section.
func (m Model) renderLogStatusBar(filtered []logEntry, innerW int, dimmed bool) string {
	left := "Type to filter"
	if m.logFilter != "" {
		left = "Filter: " + m.logFilter
		if len(filtered) == 0 {
			left += "  (no matches)"
		}
	}
	right := fmt.Sprintf("enter details  ·  %d/%d lines", len(filtered), len(m.logEntries))
	contentW := innerW - 2
	if contentW < 1 {
		contentW = 1
	}
	gap := contentW - 2 - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	content := " " + left + strings.Repeat(" ", gap) + right + " "
	content = lipgloss.NewStyle().Width(contentW).Render(content)
	if dimmed {
		return SepInactiveStyle.Render("│") + " " + InactiveStyle.Render(ansi.Strip(content)) + " " + SepInactiveStyle.Render("│")
	}
	return SepStyle.Render("│") + " " + LogStatusStyle.Render(content) + " " + SepStyle.Render("│")
}

func (m Model) renderLogDetail(bg string, filtered []logEntry) string {
	if len(filtered) == 0 {
		return bg
	}
	entry := filtered[m.selectedLogIndex(len(filtered))]
	boxW := m.width - 8
	if boxW > 96 {
		boxW = 96
	}
	if boxW < 42 {
		boxW = 42
	}
	innerW := boxW - 2
	scrollH := m.height - 14
	if scrollH < 3 {
		scrollH = 3
	}

	all := logDetailLines(entry, innerW-4)
	maxScroll := len(all) - scrollH
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := m.logDetailScroll
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	visible := all[scroll:]
	scrollbar := scrollbarCol(len(all), scrollH, scroll)

	title := " Log Detail "
	fill := innerW - lipgloss.Width(title) - 1
	if fill < 0 {
		fill = 0
	}
	lines := []string{
		SepStyle.Render("╭─") + PanelHeaderStyle.Render(title) + SepStyle.Render(strings.Repeat("─", fill)+"╮"),
	}
	lines = append(lines, m.renderLogDetailContentLine("", innerW, SepStyle.Render("│")))
	for i := range scrollH {
		text := ""
		if i < len(visible) {
			text = visible[i]
		}
		rBar := SepStyle.Render("│")
		if scrollbar != nil && i < len(scrollbar) {
			rBar = scrollbar[i]
		}
		lines = append(lines, m.renderLogDetailContentLine(text, innerW, rBar))
	}
	lines = append(lines, m.renderLogDetailContentLine("", innerW, SepStyle.Render("│")))
	lines = append(lines, m.renderLogDetailStatusBar(innerW))
	lines = append(lines, SepStyle.Render("╰"+strings.Repeat("─", innerW)+"╯"))
	return spliceOverlayLower(dimAll(bg), strings.Join(lines, "\n"), m.width, m.height)
}

func (m Model) renderLogDetailContentLine(text string, innerW int, rBar string) string {
	cell := lipgloss.NewStyle().Width(innerW - 4).Render(ansi.Truncate(text, innerW-4, ""))
	return SepStyle.Render("│") + "  " + cell + "  " + rBar
}

func (m Model) renderLogDetailStatusBar(innerW int) string {
	contentW := innerW - 2
	if contentW < 1 {
		contentW = 1
	}
	if m.logDetailCopied {
		content := StatusStyle.Render(padRight("Copied to the clipboard", contentW))
		return SepStyle.Render("│") + " " + content + " " + SepStyle.Render("│")
	}
	left := StatusKeyStyle.Render("c") + StatusStyle.Render(" copy")
	right := StatusKeyStyle.Render("esc") + StatusStyle.Render(" close")
	sep := StatusStyle.Render("  ·  ")
	plainW := lipgloss.Width("c copy  ·  esc close")
	gap := contentW - plainW
	if gap < 0 {
		gap = 0
	}
	content := left + sep + right + strings.Repeat(" ", gap)
	return SepStyle.Render("│") + " " + content + " " + SepStyle.Render("│")
}

func (m Model) currentLogDetailText() string {
	filtered := m.filteredLogEntries()
	if len(filtered) == 0 {
		return ""
	}
	entry := filtered[m.selectedLogIndex(len(filtered))]
	return entry.Raw + "\n"
}

func logDetailLines(e logEntry, width int) []string {
	if e.Msg == "" {
		if parsed, ok := parseSlogTextFields(e.Raw); ok {
			e = parsed
		}
	}
	var lines []string
	if !e.Time.IsZero() {
		lines = append(lines, logDetailField("Time", e.Time.Format(time.RFC3339)))
	}
	if e.Level != "" {
		lines = append(lines, logDetailField("Level", e.Level))
	}
	if e.Msg != "" {
		lines = append(lines, logDetailField("Message", e.Msg))
	}
	if len(e.Attrs) > 0 {
		keys := make([]string, 0, len(e.Attrs))
		for k := range e.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lines = append(lines, "", logDetailTitle("Attributes"))
		for _, k := range keys {
			lines = append(lines, logDetailGutterLines(k+"="+e.Attrs[k], width)...)
		}
	}
	lines = append(lines, "", logDetailTitle("Raw"))
	lines = append(lines, logDetailGutterLines(e.Raw, width)...)
	return lines
}

func logDetailField(label, value string) string {
	return LogDetailKeyStyle.Render(padRight(label, 9)) + value
}

func logDetailTitle(label string) string {
	return LogDetailKeyStyle.Render(label)
}

func logDetailGutterLine(value string) string {
	return LogDetailGutterStyle.Render("┊ ") + value
}

func logDetailGutterLines(value string, width int) []string {
	wrapWidth := width - 2
	if wrapWidth < 1 {
		wrapWidth = 1
	}
	wrapped := wrapPlain(value, wrapWidth)
	out := make([]string, 0, len(wrapped))
	for _, line := range wrapped {
		out = append(out, logDetailGutterLine(line))
	}
	return out
}

func parseSlogTextFields(raw string) (logEntry, bool) {
	fields := splitSlogText(raw)
	if len(fields) == 0 {
		return logEntry{}, false
	}
	out := logEntry{Raw: raw, Attrs: make(map[string]string)}
	recognised := false
	for _, field := range fields {
		k, v, ok := strings.Cut(field, "=")
		if !ok || k == "" {
			continue
		}
		v = strings.Trim(v, `"`)
		switch k {
		case "time":
			recognised = true
			out.Time, _ = time.Parse(time.RFC3339Nano, v)
		case "level":
			recognised = true
			out.Level = v
		case "msg":
			recognised = true
			out.Msg = v
		default:
			out.Attrs[k] = v
		}
	}
	return out, recognised
}

func splitSlogText(raw string) []string {
	var fields []string
	var b strings.Builder
	inQuote := false
	escaped := false
	for _, r := range raw {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\' && inQuote:
			escaped = true
			b.WriteRune(r)
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ' ' && !inQuote:
			if b.Len() > 0 {
				fields = append(fields, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		fields = append(fields, b.String())
	}
	return fields
}

func wrapPlain(s string, width int) []string {
	if width < 1 {
		return []string{s}
	}
	var out []string
	rest := s
	for lipgloss.Width(rest) > width {
		part := ansi.Truncate(rest, width, "")
		out = append(out, part)
		rest = ansi.TruncateLeft(rest, lipgloss.Width(part), "")
	}
	out = append(out, rest)
	return out
}
