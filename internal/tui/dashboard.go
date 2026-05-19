package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/golimpio/plumb/internal/monitor"
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

func (m *Model) refreshDashboard() {
	if m.globalDB == nil {
		m.globalDB, _ = stats.OpenReadOnly()
	}
	if m.globalDB == nil {
		return
	}
	globalFilter := stats.Filter{}
	m.dashLifetimeCalls = m.globalDB.TotalCalls(globalFilter)
	m.dashLifetimeSessions = m.globalDB.TotalSessions(globalFilter)
	m.dashLifetimeTokens = m.globalDB.TotalTokensSaved(globalFilter)
	m.dashLifetimeFirstAt = m.globalDB.FirstCallAt()
	m.dashLifetimeTopTools, _ = m.globalDB.Summary(globalFilter)

	if m.dashProjectFolder != "" {
		pf := stats.Filter{Workspace: m.dashProjectFolder}
		m.dashProjectCalls = m.globalDB.TotalCalls(pf)
		m.dashProjectSessions = m.globalDB.TotalSessions(pf)
		m.dashProjectTokens = m.globalDB.TotalTokensSaved(pf)
		m.dashProjectTopTools, _ = m.globalDB.Summary(pf)
	}
}

// renderDashboard renders the full-width Dashboard section (section 0).
func (m Model) renderDashboard() string {
	isOverlay := m.showHelp || m.sectionMenuOpen

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
	rBar := scrollbarCol(len(allLines), bodyHeight, m.dashScroll)

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

	final := sb.String()
	if m.showHelp {
		final = m.renderHelp(final)
	}
	if m.sectionMenuOpen {
		final = m.renderSectionMenuOverlay(final)
	}
	return final
}

// dashboardBodyLines returns all body lines for the Dashboard, ready to be
// placed between the outer border characters by renderDashboard.
func (m Model) dashboardBodyLines(width int) []string {
	var lines []string
	lines = append(lines, "")

	lines = append(lines, m.dashAlertsWidget(width)...)
	lines = append(lines, "")

	lines = append(lines, m.dashStatsRow(width)...)
	lines = append(lines, "")

	lines = append(lines, m.dashTopToolsWidget(width)...)

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

// dashRow builds a single key-value content line for use inside dashBox.
func dashRow(key, value string) string {
	return " " + KeyStyle.Width(dashKW).Render(key) + DetailStyle.Render(value)
}

// dashAlertsWidget renders the Alerts box (full width).
func (m Model) dashAlertsWidget(width int) []string {
	inner := width - 2

	type alert struct{ msg string }
	var alerts []alert

	if !daemonRunning() {
		alerts = append(alerts, alert{"Daemon is not running — start it with: plumb daemon"})
	} else if !m.daemonMetricsOK {
		alerts = append(alerts, alert{"Daemon metrics unavailable (snapshot missing or stale)"})
	}
	if m.globalDB == nil {
		alerts = append(alerts, alert{"Stats database unavailable"})
	}
	if m.loadErr != "" {
		alerts = append(alerts, alert{"Session load error: " + m.loadErr})
	}

	var content []string
	if len(alerts) == 0 {
		content = []string{" " + OkStyle.Render("✓") + " " + MutedStyle.Render("No issues detected")}
	} else {
		for _, a := range alerts {
			content = append(content, " "+WarnStyle.Render("✗")+" "+WarnStyle.Render(a.msg))
		}
	}
	return dashBox(" Alerts ", inner, content)
}

// dashStatsRow places the Lifetime, Daemon, and Activity widgets side by side
// if the terminal is wide enough, stacking them vertically otherwise.
func (m Model) dashStatsRow(width int) []string {
	lifetime := m.dashLifetimeWidget()
	daemon := m.dashDaemonWidget()
	activity := m.dashActivityWidget()
	widgets := [][]string{lifetime, daemon, activity}

	totalW := 0
	for _, w := range widgets {
		if len(w) > 0 {
			totalW += lipgloss.Width(w[0])
		}
	}
	totalW += len(widgets) - 1 // one-space gaps

	if width >= totalW {
		return joinWidgetRow(widgets)
	}
	// Vertical fallback: stack with blank lines between.
	var out []string
	for i, w := range widgets {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, w...)
	}
	return out
}

// dashLifetimeWidget renders the all-time global statistics box.
func (m Model) dashLifetimeWidget() []string {
	const (
		boxW  = 32
		inner = boxW - 2
	)
	sinceStr := "—"
	if !m.dashLifetimeFirstAt.IsZero() {
		sinceStr = m.dashLifetimeFirstAt.Format("2006-01-02")
	}
	return dashBox(" Lifetime ", inner, []string{
		dashRow("Tool Calls", formatLargeInt(m.dashLifetimeCalls)),
		dashRow("Sessions", formatLargeInt(m.dashLifetimeSessions)),
		dashRow("Tokens Saved", "~"+stats.FormatSavings(int(m.dashLifetimeTokens))),
		dashRow("Since", sinceStr),
	})
}

// dashDaemonWidget renders current daemon process metrics.
func (m Model) dashDaemonWidget() []string {
	const (
		boxW  = 46
		inner = boxW - 2
		spkW  = 14
	)
	na := MutedStyle.Render("n/a")
	pidStr := na
	memStr := na
	allocStr := na
	inuseStr := na
	sysStr := na
	gcStr := na
	gorStr := na
	cpuStr := na

	if m.daemonMetricsOK {
		d := m.daemonMetrics
		pidStr = fmt.Sprintf("%d", d.PID)
		if d.RSSAvailable {
			memStr = monitor.FormatBytes(d.RSSBytes)
		}
		allocStr = monitor.FormatBytes(d.HeapAllocBytes)
		inuseStr = monitor.FormatBytes(d.HeapInuseBytes)
		sysStr = monitor.FormatBytes(d.HeapSysBytes)
		gcStr = fmt.Sprintf("%d cycles", d.NumGC)
		gorStr = fmt.Sprintf("%d", d.Goroutines)
		if d.CPUAvailable {
			cpuStr = monitor.FormatCPU(d.CPUPercent)
		}
	}

	spark := cpuSparkline(m.daemonCPU, spkW)
	cpuLine := " " + KeyStyle.Width(dashKW).Render("CPU") +
		SelectedStyle.Render(spark) + " " + DetailStyle.Render(cpuStr)

	return dashBox(" Daemon ", inner, []string{
		dashRow("PID", pidStr),
		dashRow("Peak RSS", memStr),
		dashRow("Heap Alloc", allocStr),
		dashRow("Heap Inuse", inuseStr),
		dashRow("Heap Sys", sysStr),
		dashRow("GC", gcStr),
		dashRow("Goroutines", gorStr),
		cpuLine,
	})
}

// dashActivityWidget renders the current session activity sparkline and totals.
func (m Model) dashActivityWidget() []string {
	const (
		boxW  = 32
		inner = boxW - 2
		spkW  = 18
	)
	windowStr := "—"
	if m.activity.Window > 0 {
		windowStr = formatUptime(m.activity.Window)
	}
	spark := activitySparkline(m.activity.Buckets, spkW)
	sparkLine := " " + renderActivityGraph(spark, SelectedStyle, SepStyle)

	return dashBox(" Activity ", inner, []string{
		sparkLine,
		dashRow("Window", windowStr),
		dashRow("Calls", formatActivityCalls(m.activity.Calls)),
		dashRow("Sessions", fmt.Sprintf("%d active", len(m.sessions))),
		dashRow("Tokens (now)", "~"+stats.FormatSavings(int(m.tokenSavings))),
	})
}

// dashTopToolsWidget renders an all-time tool statistics table (full width).
func (m Model) dashTopToolsWidget(width int) []string {
	inner := width - 2

	const (
		cTool  = 22
		cCalls = 9
		cAvg   = 9
		cP95   = 9
		cErr   = 9
	)

	tools := m.dashLifetimeTopTools
	if len(tools) > 10 {
		tools = tools[:10]
	}

	header := " " + HintStyle.Width(cTool).Render("Tool") +
		HintStyle.Width(cCalls).Render("Calls") +
		HintStyle.Width(cAvg).Render("Avg ms") +
		HintStyle.Width(cP95).Render("P95 ms") +
		HintStyle.Width(cErr).Render("Errors") +
		HintStyle.Render("Tokens Saved")
	sep := " " + SepStyle.Render(strings.Repeat("╌", inner-2))

	content := []string{header, sep}
	for _, t := range tools {
		errStr := OkStyle.Render("—")
		if t.Errors > 0 {
			errStr = WarnStyle.Render(fmt.Sprintf("%d", t.Errors))
		}
		tokStr := MutedStyle.Render("—")
		if t.TokensSaved > 0 {
			tokStr = DetailStyle.Render("~" + stats.FormatSavings(int(t.TokensSaved)))
		}
		line := " " + KeyStyle.Width(cTool).Render(truncate(t.Tool, cTool-1)) +
			DetailStyle.Width(cCalls).Render(formatLargeInt(t.Calls)) +
			DetailStyle.Width(cAvg).Render(fmt.Sprintf("%.0f", t.AvgMs)) +
			DetailStyle.Width(cP95).Render(fmt.Sprintf("%d", t.P95Ms)) +
			lipgloss.NewStyle().Width(cErr).Render(errStr) +
			tokStr
		content = append(content, line)
	}
	if len(tools) == 0 {
		content = append(content, " "+MutedStyle.Render("No tool calls recorded yet"))
	}

	return dashBox(" Top Tools (all time) ", inner, content)
}

// dashProjectWidget renders stats for the detected current project (conditional).
func (m Model) dashProjectWidget(width int) []string {
	inner := width - 2

	name := filepath.Base(m.dashProjectFolder)
	if name == "" || name == "." {
		name = m.dashProjectFolder
	}

	topN := min(len(m.dashProjectTopTools), 3)
	toolNames := make([]string, 0, topN)
	for _, t := range m.dashProjectTopTools[:topN] {
		toolNames = append(toolNames, t.Tool)
	}
	topStr := strings.Join(toolNames, " · ")
	if topStr == "" {
		topStr = "—"
	}

	return dashBox(" Project: "+name+" ", inner, []string{
		dashRow("Sessions", formatLargeInt(m.dashProjectSessions)),
		dashRow("Tool Calls", formatLargeInt(m.dashProjectCalls)),
		dashRow("Tokens Saved", "~"+stats.FormatSavings(int(m.dashProjectTokens))),
		dashRow("Top Tools", topStr),
	})
}

// joinWidgetRow joins widget []string slices horizontally with a one-space gap.
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
				sb.WriteString(" ")
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
	case "ctrl+q", "ctrl+c":
		return m, tea.Quit, true
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
		m.dashScroll -= pageSize
		if m.dashScroll < 0 {
			m.dashScroll = 0
		}
	case "pgdown":
		m.dashScroll += pageSize
	default:
		return m, nil, false
	}
	return m, nil, true
}
