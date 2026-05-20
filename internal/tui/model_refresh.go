package tui

import (
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/golimpio/plumb/internal/monitor"
	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/stats"
)

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
	m.refreshDashboard()
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

func (m *Model) refreshStats() {
	m.ensureGlobalDB()
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
	if m.ctrlPath == "" || len(m.sessions) == 0 {
		return
	}
	workspace := m.sessions[m.cursor].Folder
	if workspace == "" {
		return
	}
	conn, err := net.DialTimeout("unix", m.ctrlPath, 500*time.Millisecond)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := fmt.Fprintf(conn, "diagnostics %s\n", workspace); err != nil {
		return
	}
	b, err := io.ReadAll(conn)
	if err != nil {
		return
	}
	m.lastDiagnosticsOutput = diagnosticsControlOutput(string(b))
}

func diagnosticsControlOutput(out string) string {
	if strings.HasPrefix(out, "error: unknown command \"diagnostics ") {
		return "Live diagnostics require the current daemon.\nRun `plumb stop` and reopen the TUI so plumb serve starts the current build."
	}
	return out
}

func (m *Model) refreshActivity(db *stats.DB, now time.Time) {
	if db == nil {
		m.activity = stats.ActivitySummary{}
		m.tokenSavings = 0
		return
	}
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
	window := max(now.Sub(start), time.Minute)

	activity, err := db.Activity(window, activityBuckets, stats.Filter{})
	if err != nil {
		m.activity = stats.ActivitySummary{}
		return
	}
	m.activity = activity
	m.tokenSavings = db.TotalTokensSavedSince(start, stats.Filter{})
	m.lastActivityAt = now
	m.activitySession = ""
}

func (m *Model) refreshPopupCalls() {
	if m.popupTool == "" || len(m.sessions) == 0 {
		m.popupCalls = nil
		return
	}
	if m.globalDB == nil {
		m.ensureGlobalDB()
	}
	if m.globalDB == nil {
		m.popupCalls = nil
		return
	}

	prev := selectedCallKey(m.popupCalls, m.popupCallCursor)
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
	bodyHeight := max(m.height-7, 1)
	if cursorLine >= m.popupLeftScroll+bodyHeight {
		m.popupLeftScroll = cursorLine - bodyHeight + 1
	}
	if cursorLine < m.popupLeftScroll {
		m.popupLeftScroll = cursorLine
	}
	maxScroll := max(totalLines-1, 0)
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
	if m.globalDB == nil {
		m.ensureGlobalDB()
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

func (m *Model) ensureGlobalDB() {
	if m.globalDB != nil {
		return
	}
	db, err := stats.OpenReadOnly()
	if err != nil {
		m.statsErr = err.Error()
	} else {
		m.statsErr = ""
	}
	m.globalDB = db
}
