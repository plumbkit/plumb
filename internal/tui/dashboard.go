package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/golimpio/plumb/internal/stats"
)

// detectWorkspaceFolder walks up from the current working directory looking
// for a .plumb/ marker, go.mod, pyproject.toml, or setup.py to identify the
// active project workspace.
func detectWorkspaceFolder() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		for _, marker := range []string{".plumb", "go.mod", "pyproject.toml", "setup.py"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// dashboardUptimeStart is the anchor for every "uptime"-scoped dashboard widget
// (token savings, top tools, activity). It returns the real daemon start time
// when the metrics snapshot is fresh — precise because the daemon singleton
// means every recorded call since then belongs to this run. It falls back to
// the oldest live session (then now-1m) so the TUI degrades gracefully against
// a stale/absent snapshot or an old daemon build that doesn't publish StartedAt.
func (m Model) dashboardUptimeStart(now time.Time) time.Time {
	if m.daemonMetricsOK && !m.daemonMetrics.StartedAt.IsZero() {
		return m.daemonMetrics.StartedAt
	}
	var start time.Time
	if len(m.sessions) > 0 {
		start = m.sessions[0].StartedAt
	}
	if start.IsZero() {
		start = now.Add(-time.Minute)
	}
	return start
}

func (m *Model) refreshDashboard() {
	m.ensureGlobalDB()
	if m.globalDB == nil {
		return
	}
	globalFilter := stats.Filter{}
	m.dashLifetimeCalls = m.globalDB.TotalCalls(globalFilter)
	m.dashLifetimeSessions = m.globalDB.TotalSessions(globalFilter)
	m.dashLifetimeTokens = m.globalDB.TotalTokensSaved(globalFilter)
	m.dashLifetimeFirstAt = m.globalDB.FirstCallAt()
	now := time.Now()
	chartBuckets := max(m.dashChartWidth, activityBuckets)
	if m.refreshDashboardBuckets(now, chartBuckets, globalFilter) {
		m.dashLastBucketRefresh = now
	}
	m.dashLifetimeTopTools, _ = m.globalDB.Summary(globalFilter)
	m.dashUptimeTopTools, _ = m.globalDB.Summary(stats.Filter{Since: m.dashboardUptimeStart(now)})
	m.refreshDashboardProject()
}

func (m *Model) refreshDashboardBuckets(now time.Time, chartBuckets int, gf stats.Filter) bool {
	chartWidthChanged := m.dashChartWidth != m.dashCachedChartWidth
	timedRefresh := m.dashLastBucketRefresh.IsZero() || now.Sub(m.dashLastBucketRefresh) >= dashBucketRefreshInterval
	if chartWidthChanged {
		m.dashCachedChartWidth = m.dashChartWidth
	}
	if !m.dashLifetimeFirstAt.IsZero() && (m.dashLifetimeCalls != m.dashCachedLifetimeCalls || chartWidthChanged || timedRefresh) {
		lifetimeWindow := max(time.Since(m.dashLifetimeFirstAt), time.Minute)
		if summary, err := m.globalDB.Activity(lifetimeWindow, chartBuckets, gf); err == nil {
			m.dashLifetimeBuckets = summary.Buckets
			m.dashCachedLifetimeCalls = m.dashLifetimeCalls
		}
	}
	if m.activity.Calls != m.dashCachedDaemCalls || chartWidthChanged || timedRefresh {
		m.refreshDaemBuckets(chartBuckets, gf)
	}
	return timedRefresh
}

func (m *Model) refreshDaemBuckets(chartBuckets int, gf stats.Filter) {
	if m.activity.Window > 0 {
		if summary, err := m.globalDB.Activity(m.activity.Window, chartBuckets, gf); err == nil {
			m.dashDaemBuckets = summary.Buckets
			m.dashCachedDaemCalls = m.activity.Calls
		}
	} else {
		m.dashDaemBuckets = m.activity.Buckets
		m.dashCachedDaemCalls = m.activity.Calls
	}
}

func (m *Model) refreshDashboardProject() {
	if m.dashProjectFolder == "" {
		return
	}
	pf := stats.Filter{Workspace: m.dashProjectFolder}
	m.dashProjectCalls = m.globalDB.TotalCalls(pf)
	m.dashProjectSessions = m.globalDB.TotalSessions(pf)
	m.dashProjectTokens = m.globalDB.TotalTokensSaved(pf)
	m.dashProjectTopTools, _ = m.globalDB.Summary(pf)
}

// renderDashboard renders the full-width Dashboard section (section 0).
func (m Model) renderDashboard() string {
	isOverlay := m.showHelp || m.sectionMenuOpen || m.showThemePicker

	bodyHeight := max(m.height-6, 1)
	innerW := m.width - 2

	var sb strings.Builder

	sepStyle := SepStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
	}

	// Header: 3-line logo + menu.
	logoLines := strings.Split(LogoText, "\n")
	logoW := lipgloss.Width(logoLines[0])
	menu := m.renderTopMenu(m.width-logoW, isOverlay)
	for i := range 3 {
		sb.WriteString(menu[i] + sepStyle.Render(logoLines[i]) + "\n")
	}

	// Top border integrated with the logo bottom line.
	line := "╭" + strings.Repeat("─", innerW) + "╮"
	sb.WriteString(sepStyle.Render(overlayLogoBottom(line, m.width)) + "\n")

	// Body: scrollable widget grid.
	// contentW is 6 chars narrower than innerW to leave 3-space margins on each side.
	contentW := max(innerW-6, 20)
	allLines := m.dashboardBodyLines(contentW)

	maxScroll := max(len(allLines)-bodyHeight, 0)
	if m.scrollBounds != nil {
		m.scrollBounds.maxDash = maxScroll
	}
	if m.dashScroll > maxScroll {
		m.dashScroll = maxScroll
	}
	if m.dashScroll < 0 {
		m.dashScroll = 0
	}
	visible := allLines[m.dashScroll:]
	rBar := scrollbarCol(len(allLines), bodyHeight, m.dashScroll, isOverlay)

	for i := range bodyHeight {
		line := ""
		if i < len(visible) {
			line = visible[i]
		}
		rBarChar := sepStyle.Render("│")
		if rBar != nil && i < len(rBar) {
			rBarChar = rBar[i]
		}
		// 3-space left margin; Width(innerW) pads the remaining right margin.
		padded := lipgloss.NewStyle().Width(innerW).Render("   " + line)
		if isOverlay {
			padded = InactiveStyle.Render(ansi.Strip(padded))
		}
		sb.WriteString(sepStyle.Render("│") + padded + rBarChar + "\n")
	}

	sb.WriteString(sepStyle.Render("╰"+strings.Repeat("─", innerW)+"╯") + "\n")
	sb.WriteString(m.renderMainStatusBar(isOverlay))

	return m.applyOverlays(sb.String())
}

// dashboardBodyLines returns all body lines for the Dashboard, ready to be
// placed between the outer border characters by renderDashboard.
func (m Model) dashboardBodyLines(width int) []string {
	var lines []string
	lines = append(lines, "")

	lines = append(lines, m.dashAlertsWidget(width)...)
	lines = append(lines, "")

	lines = append(lines, m.dashActivityWidget(width)...)
	lines = append(lines, "")

	lines = append(lines, m.dashStatsRow(width)...)
	lines = append(lines, "")

	lines = append(lines, m.dashTopToolsTables(width)...)

	if m.dashProjectFolder != "" && m.dashProjectCalls > 0 {
		lines = append(lines, "")
		lines = append(lines, m.dashProjectWidget(width)...)
	}

	lines = append(lines, "")
	return lines
}

// dashBox wraps content lines in a titled border box of the given inner width.
// titleText should be a short label like " Lifetime " (spaces included).
func dashBox(titleText string, innerWidth int, contentLines []string) []string {
	border := SepStyle
	title := PanelHeaderFadedStyle

	// Top border total width must equal innerWidth+2 (same as content and bottom lines).
	// Structure: "╭─" (2) + titleText + fill×"─" + "╮" (1) = inner+2 → fill = inner-1-len(title).
	topFill := max(innerWidth-1-lipgloss.Width(titleText), 0)
	out := make([]string, 0, len(contentLines)+4)
	out = append(out, border.Render("╭─")+title.Render(titleText)+border.Render(strings.Repeat("─", topFill)+"╮"))

	// Top padding
	out = append(out, border.Render("│")+strings.Repeat(" ", innerWidth)+border.Render("│"))

	for _, l := range contentLines {
		padW := max(innerWidth-lipgloss.Width(l), 0)
		out = append(out, border.Render("│")+l+strings.Repeat(" ", padW)+border.Render("│"))
	}

	// Bottom padding
	out = append(out, border.Render("│")+strings.Repeat(" ", innerWidth)+border.Render("│"))
	out = append(out, border.Render("╰"+strings.Repeat("─", innerWidth)+"╯"))
	return out
}

const dashKW = 13 // key column width inside widget rows

const dashWidgetGap = 3

// joinWidgetRow joins widget []string slices horizontally with a fixed gap.
// Shorter widgets are padded with blank lines to match the tallest.
func joinWidgetRow(widgets [][]string) []string {
	maxH := 0
	for _, w := range widgets {
		if len(w) > maxH {
			maxH = len(w)
		}
	}
	widths := make([]int, len(widgets))
	for i, w := range widgets {
		if len(w) > 0 {
			widths[i] = lipgloss.Width(w[0])
		}
	}
	out := make([]string, maxH)
	for row := range maxH {
		var sb strings.Builder
		for wi, w := range widgets {
			if wi > 0 {
				sb.WriteString(strings.Repeat(" ", dashWidgetGap))
			}
			if row < len(w) {
				sb.WriteString(w[row])
			} else {
				sb.WriteString(strings.Repeat(" ", widths[wi]))
			}
		}
		out[row] = sb.String()
	}
	return out
}

// padBoxHeight grows a dashBox-rendered box to target total lines by inserting blank
// bordered rows just above the bottom border, so side-by-side boxes share one frame height.
// It reuses the box's own bottom-padding line, so the inserted rows are styled and sized to
// match. A box with fewer than two lines, or already at/above target, is returned unchanged.
func padBoxHeight(box []string, target int) []string {
	if len(box) < 2 || len(box) >= target {
		return box
	}
	blank := box[len(box)-2] // bottom-padding line: │  …  │
	out := make([]string, 0, target)
	out = append(out, box[:len(box)-1]...) // all but the bottom border
	for range target - len(box) {
		out = append(out, blank) // extra empty bordered rows inside the frame
	}
	out = append(out, box[len(box)-1]) // bottom border, last
	return out
}

// formatLargeInt formats n as a short human string: 1234 → "1.2k", 1200000 → "1.2m".
func formatLargeInt(n int64) string {
	switch {
	case n >= 1_000_000:
		v := float64(n) / 1_000_000
		if v == float64(int64(v)) {
			return fmt.Sprintf("%.0fm", v)
		}
		return fmt.Sprintf("%.1fm", v)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// handleDashboardKey handles key input while the Dashboard section is active.
// Returns the updated model and command, or (zero, nil) if the key was not
// handled (caller should continue with the main key switch).
func (m Model) handleDashboardKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if m.currentSection != 0 || m.sectionMenuOpen || m.showHelp {
		return m, nil, false
	}
	pageSize := max(m.height-6, 1)
	switch msg.String() {
	case "ctrl+q":
		return m, tea.Quit, true
	case "ctrl+c":
		m, cmd := m.mainKeyQuit()
		return m, cmd, true
	case "esc":
		// nothing to dismiss in dashboard
	case "/":
		m.sectionMenuOpen = true
		m.sectionMenuCursor = m.currentSection
	case "ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5":
		m.selectSectionShortcut(msg.String())
	case "ctrl+h":
		m.showHelp = true
	case "up", "k":
		if m.dashScroll > 0 {
			m.dashScroll--
		}
	case "down", "j":
		m.dashScroll++
	case "pgup":
		m = m.dashKeyPageUp(pageSize)
	case "pgdown":
		m.dashScroll += pageSize
	default:
		return m, nil, false
	}
	return m, nil, true
}

func (m Model) dashKeyPageUp(pageSize int) Model {
	m.dashScroll -= pageSize
	if m.dashScroll < 0 {
		m.dashScroll = 0
	}
	return m
}
