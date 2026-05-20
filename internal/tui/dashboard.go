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

func (m Model) dashboardUptimeStart(now time.Time) time.Time {
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
	chartBuckets := max(m.dashChartWidth, activityBuckets)
	if !m.dashLifetimeFirstAt.IsZero() {
		lifetimeWindow := max(time.Since(m.dashLifetimeFirstAt), time.Minute)
		if summary, err := m.globalDB.Activity(lifetimeWindow, chartBuckets, globalFilter); err == nil {
			m.dashLifetimeBuckets = summary.Buckets
		}
	}
	if m.activity.Window > 0 {
		if summary, err := m.globalDB.Activity(m.activity.Window, chartBuckets, globalFilter); err == nil {
			m.dashDaemBuckets = summary.Buckets
		}
	} else {
		m.dashDaemBuckets = m.activity.Buckets
	}
	m.dashLifetimeTopTools, _ = m.globalDB.Summary(globalFilter)
	m.dashUptimeTopTools, _ = m.globalDB.Summary(stats.Filter{Since: m.dashboardUptimeStart(time.Now())})

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

// dashRow builds a single key-value content line for use inside dashBox.
func dashRow(key, value string) string {
	return " " + KeyStyle.Width(dashKW).Render(key) + DetailStyle.Render(value)
}

// dashAlertsWidget renders the Alerts box (full width).
func (m Model) dashAlertsWidget(width int) []string {
	inner := width - 2
	alerts := m.dashboardAlerts()

	var content []string
	if len(alerts) == 0 {
		content = []string{"   " + OkStyle.Render("✓") + " " + MutedStyle.Render("No issues detected") + "   "}
	} else {
		for _, msg := range alerts {
			content = append(content, "   "+WarnStyle.Render("✗")+" "+WarnStyle.Render(msg)+"   ")
		}
	}
	return dashBox(" Alerts ", inner, content)
}

func (m Model) dashboardAlerts() []string {
	var alerts []string
	if !daemonRunning() {
		alerts = append(alerts, "Daemon is not running; start it with: plumb daemon")
	} else if !m.daemonMetricsOK {
		alerts = append(alerts, "Daemon metrics unavailable; snapshot missing or stale")
	}
	if m.loadErr != "" {
		alerts = append(alerts, "Session load error: "+m.loadErr)
	}
	if m.statsErr != "" {
		alerts = append(alerts, "Stats database unavailable: "+m.statsErr)
	}
	if m.dashProjectFolder == "" {
		alerts = append(alerts, "No workspace resolved; run plumb init in this project")
	}
	if msg := m.dashboardDaemonVersionAlert(); msg != "" {
		alerts = append(alerts, msg)
	}
	if msg := m.dashboardWorkspaceStateAlert(); msg != "" {
		alerts = append(alerts, msg)
	}
	if msg := m.dashboardErrorSpikeAlert(); msg != "" {
		alerts = append(alerts, msg)
	}
	return alerts
}

func (m Model) dashboardDaemonVersionAlert() string {
	if Version == "" || Version == "dev" {
		return ""
	}
	for _, s := range m.sessions {
		if s.DaemonVersion != "" && s.DaemonVersion != Version {
			return fmt.Sprintf("Daemon version mismatch: running %s, TUI %s; run plumb stop", s.DaemonVersion, Version)
		}
	}
	return ""
}

func (m Model) dashboardWorkspaceStateAlert() string {
	for _, s := range m.sessions {
		if s.Synthetic {
			return "Workspace auto-attached; run plumb init to make it explicit"
		}
	}
	for _, s := range m.sessions {
		if m.dashProjectFolder != "" && s.Folder != m.dashProjectFolder {
			continue
		}
		if s.Language == "" || s.Language == "none" {
			return "LSP unavailable for this workspace; filesystem tools still work"
		}
	}
	return ""
}

func (m Model) dashboardErrorSpikeAlert() string {
	var calls, errors int64
	for _, t := range m.dashUptimeTopTools {
		calls += t.Calls
		errors += t.Errors
	}
	if calls < 10 || errors < 3 || errors*100 < calls*20 {
		return ""
	}
	return fmt.Sprintf("Recent tool error spike: %d/%d uptime calls failed", errors, calls)
}

// dashStatsRow places the Daemon and Tokens Saved widgets side by side
// if the terminal is wide enough, stacking them vertically otherwise.
func (m Model) dashStatsRow(width int) []string {
	daemonInner := dashDaemonMinInner
	tokenInner := dashTokensMinInner
	minRowW := daemonInner + 2 + dashWidgetGap + tokenInner + 2
	if width >= minRowW {
		extra := width - minRowW
		desiredTokenInner := tokenInner + extra
		tokenInner = dashTokenInnerForGroups(dashTokenGroupsForInner(desiredTokenInner))
		daemonInner += desiredTokenInner - tokenInner
	}

	daemon := m.dashDaemonWidget(daemonInner)
	tokens := m.dashTokensWidget(tokenInner)
	widgets := [][]string{daemon, tokens}

	totalW := 0
	for _, w := range widgets {
		if len(w) > 0 {
			totalW += lipgloss.Width(w[0])
		}
	}
	totalW += (len(widgets) - 1) * dashWidgetGap

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

const dashDaemonMinInner = 44

// dashDaemonWidget renders current daemon memory metrics.
func (m Model) dashDaemonWidget(inner int) []string {
	inner = max(inner, dashDaemonMinInner)

	na := "n/a"
	pidStr := na
	memStr := na
	allocStr := na
	inuseStr := na
	sysStr := na
	gcStr := na
	gorStr := na

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
	}

	titleText := " Daemon Memory "
	if pidStr != na {
		titleText = fmt.Sprintf(" Daemon Memory (PID %s) ", pidStr)
	}

	return dashBox(titleText, inner, []string{
		dashMemoryRow("Peak RSS", memStr, inner),
		dashMemoryRow("Heap Alloc", allocStr, inner),
		dashMemoryRow("Heap In Use", inuseStr, inner),
		dashMemoryRow("Heap Sys", sysStr, inner),
		dashMemoryRow("GC", gcStr, inner),
		dashMemoryRow("Goroutines", gorStr, inner),
	})
}

func dashMemoryRow(label, value string, inner int) string {
	const margin = 3
	leaderW := max(inner-margin-lipgloss.Width(label)-1-1-lipgloss.Width(value)-margin, 1)
	labelStyle := lipgloss.NewStyle().Foreground(ActiveTheme.Key)
	return strings.Repeat(" ", margin) +
		labelStyle.Render(label) + " " +
		SepStyle.Render(strings.Repeat("⣀", leaderW)) + " " +
		DetailStyle.Render(value) + strings.Repeat(" ", margin)
}

func dashTokenGroupsForInner(inner int) int {
	const (
		margin = 3
		gap    = 3
	)
	return max(((inner-margin*2-gap)/2+1)/3, 1)
}

func dashTokenInnerForGroups(groups int) int {
	const (
		margin = 3
		gap    = 3
	)
	blockW := groups*2 + groups - 1
	return margin*2 + gap + blockW*2
}

const dashTokensMinInner = 55

// dashTokensWidget renders both all-time and current-daemon token savings.
func (m Model) dashTokensWidget(inner int) []string {
	const (
		margin = 3
		gap    = 3
		blockH = 4
	)
	inner = max(inner, dashTokensMinInner)
	groups := dashTokenGroupsForInner(inner)
	inner = dashTokenInnerForGroups(groups)
	blockW := groups*2 + groups - 1

	uptimeLabel := "uptime " + stats.FormatSavings(int(m.tokenSavings))
	if m.activity.Window > 0 {
		uptimeLabel += " (" + formatUptime(m.activity.Window) + ")"
	}
	totalLabel := "total " + stats.FormatSavings(int(m.dashLifetimeTokens))
	if !m.dashLifetimeFirstAt.IsZero() {
		totalLabel += " (" + formatUptimePrecise(time.Since(m.dashLifetimeFirstAt)) + ")"
	}

	leftBlocks := tokenSavingsBlockRow(m.tokenSavings, groups)
	rightBlocks := tokenSavingsBlockRow(m.dashLifetimeTokens, groups)
	blockLine := strings.Repeat(" ", margin) + leftBlocks + strings.Repeat(" ", gap) + rightBlocks + strings.Repeat(" ", margin)
	labelGap := max(blockW+gap-lipgloss.Width(uptimeLabel), 1)
	labelLine := strings.Repeat(" ", margin) + DetailStyle.Render(uptimeLabel) + strings.Repeat(" ", labelGap) + DetailStyle.Render(totalLabel)

	content := make([]string, 0, blockH+2)
	for range blockH {
		content = append(content, blockLine)
	}
	content = append(content, "", labelLine)

	return dashBox(" Tokens Saved ", inner, content)
}

func tokenSavingsBlockRow(tokens int64, groups int) string {
	filled, _ := tokenSavingsBar(tokens, groups)
	filledGroups := lipgloss.Width(filled)
	parts := make([]string, 0, groups)
	for i := range groups {
		block := "▆▆"
		if i < filledGroups {
			parts = append(parts, SelectedStyle.Render(block))
		} else {
			parts = append(parts, SepStyle.Render(block))
		}
	}
	return strings.Join(parts, " ")
}

// dashTopToolsTables renders all-time and uptime tool statistics as dashboard widgets.
func (m Model) dashTopToolsTables(width int) []string {
	allTime := dashTopToolsWidget(" Top Tools (all time) ", width-2, m.dashLifetimeTopTools, dashTopToolsAllTime)
	if !hasToolStats(m.dashUptimeTopTools) {
		return allTime
	}

	uptime := dashTopToolsWidget(" Top Tools (uptime) ", width-2, m.dashUptimeTopTools, dashTopToolsUptime)
	if width >= 112 {
		outer := (width - dashWidgetGap) / 2
		leftInner := max(outer-2, 40)
		rightInner := max(width-dashWidgetGap-outer-2, 40)
		return joinWidgetRow([][]string{
			dashTopToolsWidget(" Top Tools (all time) ", leftInner, m.dashLifetimeTopTools, dashTopToolsAllTime),
			dashTopToolsWidget(" Top Tools (uptime) ", rightInner, m.dashUptimeTopTools, dashTopToolsUptime),
		})
	}

	lines := allTime
	lines = append(lines, "")
	lines = append(lines, uptime...)
	return lines
}

type dashTopToolsWidgetKind int

const (
	dashTopToolsAllTime dashTopToolsWidgetKind = iota
	dashTopToolsUptime
)

func dashTopToolsWidget(title string, inner int, tools []stats.ToolStat, kind dashTopToolsWidgetKind) []string {
	inner = max(inner, 40)
	content := dashCompactTopToolsTable(max(inner-6, 20), tools, kind)
	for i, line := range content {
		content[i] = "   " + line + "   "
	}
	return dashBox(title, inner, content)
}

func dashCompactTopToolsTable(width int, tools []stats.ToolStat, kind dashTopToolsWidgetKind) []string {
	const (
		callsW  = 9
		metricW = 12
	)
	toolW := max(width-callsW-metricW, 12)
	metricHeader := "Tokens"
	if kind == dashTopToolsUptime {
		metricHeader = "Errors"
	}
	if len(tools) > 10 {
		tools = tools[:10]
	}

	header := HintStyle.Width(toolW).Render("Tool") +
		HintStyle.Width(callsW).Align(lipgloss.Right).Render("Calls") +
		HintStyle.Width(metricW).Align(lipgloss.Right).Render(metricHeader)
	sep := SepStyle.Render(strings.Repeat("╌", width))

	content := []string{header, sep}
	for _, t := range tools {
		metric := MutedStyle.Render("—")
		if kind == dashTopToolsAllTime && t.TokensSaved > 0 {
			metric = DetailStyle.Render("~" + stats.FormatSavings(int(t.TokensSaved)))
		}
		if kind == dashTopToolsUptime && t.Errors > 0 {
			metric = WarnStyle.Render(fmt.Sprintf("%d", t.Errors))
		}
		line := KeyStyle.Width(toolW).Render(truncate(t.Tool, toolW-1)) +
			DetailStyle.Width(callsW).Align(lipgloss.Right).Render(formatLargeInt(t.Calls)) +
			lipgloss.NewStyle().Width(metricW).Align(lipgloss.Right).Render(metric)
		content = append(content, line)
	}
	if len(tools) == 0 {
		content = append(content, MutedStyle.Render("No tool calls recorded yet"))
	}
	return content
}

func hasToolStats(tools []stats.ToolStat) bool {
	for _, t := range tools {
		if t.Calls > 0 || t.AvgMs > 0 || t.P95Ms > 0 || t.Errors > 0 || t.TokensSaved > 0 {
			return true
		}
	}
	return false
}

func dashTopToolsTable(title string, width int, tools []stats.ToolStat) []string {
	const (
		minTool   = 18
		minNumber = 7
		minTokens = 14
	)
	cTool := max(width*40/100, minTool)
	remaining := max(width-cTool, minNumber*4+minTokens)
	cCalls := max(remaining*12/60, minNumber)
	cAvg := max(remaining*12/60, minNumber)
	cP95 := max(remaining*12/60, minNumber)
	cErr := max(remaining*12/60, minNumber)
	cTokens := max(width-cTool-cCalls-cAvg-cP95-cErr, minTokens)
	cTool = max(width-cCalls-cAvg-cP95-cErr-cTokens, minTool)

	if len(tools) > 10 {
		tools = tools[:10]
	}

	header := HintStyle.Width(cTool).Render(title) +
		HintStyle.Width(cCalls).Align(lipgloss.Right).Render("Calls") +
		HintStyle.Width(cAvg).Align(lipgloss.Right).Render("Avg ms") +
		HintStyle.Width(cP95).Align(lipgloss.Right).Render("P95 ms") +
		HintStyle.Width(cErr).Align(lipgloss.Right).Render("Errors") +
		HintStyle.Width(cTokens).Align(lipgloss.Right).Render("Tokens")
	sep := SepStyle.Render(strings.Repeat("╌", max(width, lipgloss.Width(ansi.Strip(header)))))

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
		line := KeyStyle.Width(cTool).Render(truncate(t.Tool, cTool-1)) +
			DetailStyle.Width(cCalls).Align(lipgloss.Right).Render(formatLargeInt(t.Calls)) +
			DetailStyle.Width(cAvg).Align(lipgloss.Right).Render(fmt.Sprintf("%.0f", t.AvgMs)) +
			DetailStyle.Width(cP95).Align(lipgloss.Right).Render(fmt.Sprintf("%d", t.P95Ms)) +
			lipgloss.NewStyle().Width(cErr).Align(lipgloss.Right).Render(errStr) +
			lipgloss.NewStyle().Width(cTokens).Align(lipgloss.Right).Render(tokStr)
		content = append(content, line)
	}
	if len(tools) == 0 {
		content = append(content, MutedStyle.Render("No tool calls recorded yet"))
	}
	return content
}

// dashProjectWidget renders stats for the detected current project (conditional).
func (m Model) dashProjectWidget(width int) []string {
	inner := width - 2

	name := filepath.Base(m.dashProjectFolder)
	if name == "" || name == "." {
		name = m.dashProjectFolder
	}

	content := []string{
		dashProjectMetricRow("Sessions", formatLargeInt(m.dashProjectSessions), m.dashProjectSessions, m.dashLifetimeSessions, inner),
		dashProjectMetricRow("Tool Calls", formatLargeInt(m.dashProjectCalls), m.dashProjectCalls, m.dashLifetimeCalls, inner),
		dashProjectMetricRow("Tokens Saved", "~"+stats.FormatSavings(int(m.dashProjectTokens)), m.dashProjectTokens, m.dashLifetimeTokens, inner),
		"",
	}
	for _, line := range dashTopToolsTable("Top Tools", max(inner-6, 20), m.dashProjectTopTools) {
		content = append(content, "   "+line+"   ")
	}

	return dashBox(" Project: "+name+" ", inner, content)
}

func dashProjectMetricRow(label, value string, numerator, denominator int64, inner int) string {
	const (
		margin = 3
		labelW = dashKW
		valueW = 14
	)
	pct := ratioPercent(numerator, denominator)
	barW := max(inner-margin*2-labelW-valueW-2, 1)
	fill, empty := ratioBar(pct, barW)
	valueText := fmt.Sprintf("%s (%d%%)", value, pct)
	return strings.Repeat(" ", margin) +
		KeyStyle.Width(labelW).Render(label) + " " +
		SelectedStyle.Render(fill) + SepStyle.Render(empty) + " " +
		DetailStyle.Width(valueW).Align(lipgloss.Right).Render(valueText) +
		strings.Repeat(" ", margin)
}

func ratioPercent(numerator, denominator int64) int {
	if numerator <= 0 || denominator <= 0 {
		return 0
	}
	pct := int((numerator*100 + denominator/2) / denominator)
	return min(max(pct, 0), 100)
}

func ratioBar(percent, width int) (string, string) {
	if width <= 0 {
		return "", ""
	}
	filled := percent * width / 100
	if percent > 0 && filled == 0 {
		filled = 1
	}
	filled = min(max(filled, 0), width)
	return strings.Repeat("■", filled), strings.Repeat("■", width-filled)
}

func formatFriendlySinceDate(t, now time.Time) string {
	if t.Year() == now.Year() {
		return t.Format("2 Jan")
	}
	return t.Format("2 Jan 2006")
}

// dashActivityWidget renders the activity chart as a full-width dashboard widget.
func (m Model) dashActivityWidget(width int) []string {
	inner := max(width-2, 1)
	chartW := max(inner-6, 1)
	topL, topR, botL, botR := m.dashActivityCaptions(time.Now())

	out := []string{
		dashActivityBorder("╭", "╮", inner, topL, topR),
		SepStyle.Render("│") + strings.Repeat(" ", inner) + SepStyle.Render("│"),
	}
	for _, line := range m.dashActivityGraphLines(chartW) {
		padW := max(inner-3-lipgloss.Width(line), 0)
		out = append(out, SepStyle.Render("│")+"   "+line+strings.Repeat(" ", padW)+SepStyle.Render("│"))
	}
	out = append(out,
		SepStyle.Render("│")+strings.Repeat(" ", inner)+SepStyle.Render("│"),
		dashActivityBorder("╰", "╯", inner, botL, botR),
	)
	return out
}

func (m Model) dashActivityCaptions(now time.Time) (string, string, string, string) {
	callScope := "all time"
	topRight := ""
	if !m.dashLifetimeFirstAt.IsZero() {
		callScope = "since " + formatFriendlySinceDate(m.dashLifetimeFirstAt, now)
		topRight = formatUptimePrecise(now.Sub(m.dashLifetimeFirstAt))
	}
	topLeft := "↓ " + formatLargeInt(m.dashLifetimeCalls) + " calls (" + callScope + ") · " + formatSessionCount(m.dashLifetimeSessions)

	bottomLeft := "↑ " + formatActivityCalls(m.activity.Calls) + " (uptime) · " + formatActiveSessionCount(int64(len(m.sessions)))
	bottomRight := ""
	if m.activity.Window > 0 {
		bottomRight = formatUptime(m.activity.Window)
	}
	return topLeft, topRight, bottomLeft, bottomRight
}

func dashActivityBorder(leftCorner, rightCorner string, inner int, leftTitle, rightTitle string) string {
	border := SepStyle
	title := PanelHeaderFadedStyle
	rightText := ""
	if rightTitle != "" {
		rightText = " " + rightTitle + " "
	}
	availableLeft := max(inner-2-lipgloss.Width(rightText)-2, 0)
	if lipgloss.Width(leftTitle) > availableLeft {
		leftTitle = ansi.Truncate(leftTitle, availableLeft, "…")
	}
	leftText := " " + leftTitle + " "
	fillW := max(inner-2-lipgloss.Width(leftText)-lipgloss.Width(rightText), 0)
	return border.Render(leftCorner+"─") +
		title.Render(leftText) +
		border.Render(strings.Repeat("─", fillW)) +
		title.Render(rightText) +
		border.Render("─"+rightCorner)
}

// dashActivityGraphLines renders a 4-row borderless braille area chart of tool-call
// activity. Top 2 rows show lifetime history (bottom-fill, ⣀ idle background).
// Bottom 2 rows show daemon history (top-fill, ⠉ idle background). Together they
// form a dotted centre-line when the chart has no activity.
func (m Model) dashActivityGraphLines(width int) []string {
	const halfH = 2 // chart rows per half

	// Braille bottom-fill patterns: left/right column filled upward.
	botL := [5]int{0, 0x40, 0x44, 0x46, 0x47}
	botR := [5]int{0, 0x80, 0xA0, 0xB0, 0xB8}

	// Braille top-fill patterns: left/right column filled downward.
	topL := [5]int{0, 0x01, 0x03, 0x07, 0x47}
	topR := [5]int{0, 0x08, 0x18, 0x38, 0xB8}

	buildGrid := func(buckets []int64, fillDown bool) [][]int {
		pixH := halfH * 4
		var maxV int64 = 10
		for _, v := range buckets {
			if v > maxV {
				maxV = v
			}
		}
		sample := func(i int) int64 {
			if len(buckets) == 0 {
				return 0
			}
			idx := i * len(buckets) / (width * 2)
			if idx >= len(buckets) {
				idx = len(buckets) - 1
			}
			return buckets[idx]
		}
		toPx := func(v int64) int {
			if v <= 0 {
				return 0
			}
			px := int(float64(v) / float64(maxV) * float64(pixH-1))
			if px < 1 {
				px = 1
			}
			return px
		}
		grid := make([][]int, halfH)
		for r := range halfH {
			grid[r] = make([]int, width)
		}
		for x := range width {
			pxL := toPx(sample(x * 2))
			pxR := toPx(sample(x*2 + 1))
			if fillDown {
				for r := range halfH {
					base := r * 4
					lf := min(4, max(0, pxL-base))
					rf := min(4, max(0, pxR-base))
					grid[r][x] = topL[lf] | topR[rf]
				}
			} else {
				for r := halfH - 1; r >= 0; r-- {
					base := (halfH - 1 - r) * 4
					lf := min(4, max(0, pxL-base))
					rf := min(4, max(0, pxR-base))
					grid[r][x] = botL[lf] | botR[rf]
				}
			}
		}
		return grid
	}

	renderRow := func(row []int, bgRune rune) string {
		var sb strings.Builder
		i := 0
		for i < width {
			faded := row[i] == 0
			j := i + 1
			for j < width && (row[j] == 0) == faded {
				j++
			}
			var run strings.Builder
			for k := i; k < j; k++ {
				if faded {
					run.WriteRune(bgRune)
				} else {
					run.WriteRune(rune(0x2800 + row[k]))
				}
			}
			if faded {
				sb.WriteString(SepStyle.Render(run.String()))
			} else {
				sb.WriteString(SelectedStyle.Render(run.String()))
			}
			i = j
		}
		return sb.String()
	}

	gridLife := buildGrid(m.dashLifetimeBuckets, false) // bottom-fill
	gridDaem := buildGrid(m.dashDaemBuckets, true)      // top-fill

	lines := make([]string, 0, halfH*2)

	// Top half: lifetime data, bottom-fill.
	// Only the innermost row (r == halfH-1) shows ⣀ when idle; the outer row is blank.
	for r := range halfH {
		bg := rune(' ')
		if r == halfH-1 {
			bg = '⣀'
		}
		lines = append(lines, renderRow(gridLife[r], bg))
	}

	// Bottom half: daemon data, top-fill.
	// Only the innermost row (r == 0) shows ⠉ when idle; the outer row is blank.
	for r := range halfH {
		bg := rune(' ')
		if r == 0 {
			bg = '⠉'
		}
		lines = append(lines, renderRow(gridDaem[r], bg))
	}

	return lines
}

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
