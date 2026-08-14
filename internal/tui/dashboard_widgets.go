package tui

import (
	"cmp"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/textfmt"
)

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
	relStr := na
	gcStr := na
	gorStr := na

	if m.daemonMetricsOK {
		d := m.daemonMetrics
		pidStr = strconv.Itoa(d.PID)
		if d.RSSAvailable {
			memStr = textfmt.HumanBytesCompact(d.RSSBytes)
		}
		allocStr = textfmt.HumanBytesCompact(d.HeapAllocBytes)
		inuseStr = textfmt.HumanBytesCompact(d.HeapInuseBytes)
		sysStr = textfmt.HumanBytesCompact(d.HeapSysBytes)
		relStr = textfmt.HumanBytesCompact(d.HeapReleasedBytes)
		gcStr = fmt.Sprintf("%d cycles", d.NumGC)
		gorStr = strconv.Itoa(d.Goroutines)
	}

	titleText := " Daemon Memory "
	if pidStr != na {
		titleText = fmt.Sprintf(" Daemon Memory (PID %s) ", pidStr)
	}

	return dashBox(titleText, inner, []string{
		dashMemoryRow("RSS", memStr, inner),
		dashMemoryRow("Heap Alloc", allocStr, inner),
		dashMemoryRow("Heap In Use", inuseStr, inner),
		dashMemoryRow("Heap Sys", sysStr, inner),
		dashMemoryRow("Heap Released", relStr, inner),
		dashMemoryRow("GC", gcStr, inner),
		dashMemoryRow("Goroutines", gorStr, inner),
	})
}

func dashMemoryRow(label, value string, inner int) string {
	const margin = 3
	leaderW := max(inner-margin-lipgloss.Width(label)-1-1-lipgloss.Width(value)-margin, 1)
	return strings.Repeat(" ", margin) +
		DashLabelStyle.Render(label) + " " +
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
	// The lifetime total is just capability + efficiency, so the right-hand block
	// shows the honest split between the two axes rather than progress toward a
	// fixed target (which lifetime totals always saturate). Colour each axis label
	// to match its columns in the block below: capability green, efficiency accent.
	capPart := "cap ~" + stats.FormatSavings(int(m.dashLifetimeAxes.Capability))
	effPart := "eff ~" + stats.FormatSavings(int(m.dashLifetimeAxes.Efficiency))
	rightLabel := capPart + " · " + effPart
	rightStyled := OkStyle.Render(capPart) + DetailStyle.Render(" · ") + SelectedStyle.Render(effPart)
	if !m.dashLifetimeFirstAt.IsZero() {
		suffix := " (" + formatUptimePrecise(time.Since(m.dashLifetimeFirstAt)) + ")"
		rightLabel += suffix
		rightStyled += DetailStyle.Render(suffix)
	}

	leftBlocks := tokenSavingsBlockRow(m.tokenSavings, groups)
	rightBlocks := tokenSavingsSplitRow(m.dashLifetimeAxes, groups)
	blockLine := strings.Repeat(" ", margin) + leftBlocks + strings.Repeat(" ", gap) + rightBlocks + strings.Repeat(" ", margin)
	labelGap := max(blockW+gap-lipgloss.Width(uptimeLabel), 1)
	// dashBox does not widen for an over-long line; clamp the gap so the caption
	// stays within inner at narrow widths instead of overflowing the right border.
	if over := margin + lipgloss.Width(uptimeLabel) + labelGap + lipgloss.Width(rightLabel) - inner; over > 0 {
		labelGap = max(labelGap-over, 1)
	}
	labelLine := strings.Repeat(" ", margin) + DetailStyle.Render(uptimeLabel) + strings.Repeat(" ", labelGap) + rightStyled

	content := make([]string, 0, blockH+2)
	for range blockH {
		content = append(content, blockLine)
	}
	content = append(content, "", labelLine)

	return dashBox(" Token Efficiency ", inner, content)
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

// splitGroups apportions `groups` filled columns between the capability and
// efficiency savings axes by their share of the total. A non-zero axis always
// gets at least one column (mirroring tokenSavingsBar's "show at least one"),
// and the two counts always sum to groups. A zero total returns (0, 0) so the
// caller can render the row as empty.
func splitGroups(capTokens, effTokens int64, groups int) (capGroups, effGroups int) {
	total := capTokens + effTokens
	if groups <= 0 || total <= 0 {
		return 0, 0
	}
	capGroups = int(math.Round(float64(capTokens) / float64(total) * float64(groups)))
	if capGroups < 0 {
		capGroups = 0
	}
	if capGroups > groups {
		capGroups = groups
	}
	if capTokens > 0 && capGroups == 0 {
		capGroups = 1
	}
	if effTokens > 0 && capGroups == groups {
		capGroups = groups - 1
	}
	return capGroups, groups - capGroups
}

// tokenSavingsSplitRow renders a full row of `groups` lit blocks split by the
// capability/efficiency composition of all-time savings: capability columns in
// the success colour, efficiency columns in the accent colour. Unlike the
// progress-style left block, every column is lit — the row shows a ratio, not
// progress toward a target. With no savings recorded yet it renders a dim row.
func tokenSavingsSplitRow(axes stats.AxisTotals, groups int) string {
	capGroups, effGroups := splitGroups(axes.Capability, axes.Efficiency, groups)
	const block = "▆▆"
	parts := make([]string, 0, groups)
	if capGroups == 0 && effGroups == 0 {
		for range groups {
			parts = append(parts, SepStyle.Render(block))
		}
		return strings.Join(parts, " ")
	}
	for range capGroups {
		parts = append(parts, OkStyle.Render(block))
	}
	for range effGroups {
		parts = append(parts, SelectedStyle.Render(block))
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
		left := dashTopToolsWidget(" Top Tools (all time) ", leftInner, m.dashLifetimeTopTools, dashTopToolsAllTime)
		right := dashTopToolsWidget(" Top Tools (uptime) ", rightInner, m.dashUptimeTopTools, dashTopToolsUptime)
		h := max(len(left), len(right))
		return joinWidgetRow([][]string{padBoxHeight(left, h), padBoxHeight(right, h)})
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
	metricHeader := "Efficiency"
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
		if eff := t.CapabilityTokens + t.EfficiencyTokens; kind == dashTopToolsAllTime && eff > 0 {
			metric = DetailStyle.Render("~" + stats.FormatSavings(int(eff)))
		}
		if kind == dashTopToolsUptime && t.Errors > 0 {
			metric = WarnStyle.Render(strconv.FormatInt(t.Errors, 10))
		}
		line := KeyStyle.Width(toolW).Render(textfmt.Ellipsis(t.Tool, toolW-1)) +
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
		HintStyle.Width(cTokens).Align(lipgloss.Right).Render("Efficiency")
	sep := SepStyle.Render(strings.Repeat("╌", max(width, lipgloss.Width(ansi.Strip(header)))))

	content := []string{header, sep}
	for _, t := range tools {
		errStr := OkStyle.Render("—")
		if t.Errors > 0 {
			errStr = WarnStyle.Render(strconv.FormatInt(t.Errors, 10))
		}
		tokStr := MutedStyle.Render("—")
		if eff := t.CapabilityTokens + t.EfficiencyTokens; eff > 0 {
			tokStr = DetailStyle.Render("~" + stats.FormatSavings(int(eff)))
		}
		line := KeyStyle.Width(cTool).Render(textfmt.Ellipsis(t.Tool, cTool-1)) +
			DetailStyle.Width(cCalls).Align(lipgloss.Right).Render(formatLargeInt(t.Calls)) +
			DetailStyle.Width(cAvg).Align(lipgloss.Right).Render(fmt.Sprintf("%.0f", t.AvgMs)) +
			DetailStyle.Width(cP95).Align(lipgloss.Right).Render(strconv.FormatInt(t.P95Ms, 10)) +
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

	tableLines := dashTopToolsTable("Top Tools", max(inner-6, 20), m.dashProjectTopTools)
	content := make([]string, 0, 4+len(tableLines))
	content = append(content,
		dashProjectMetricRow("Sessions", formatLargeInt(m.dashProjectSessions), m.dashProjectSessions, m.dashLifetimeSessions, inner),
		dashProjectMetricRow("Tool Calls", formatLargeInt(m.dashProjectCalls), m.dashProjectCalls, m.dashLifetimeCalls, inner),
		dashProjectMetricRow("Efficiency", "~"+stats.FormatSavings(int(m.dashProjectTokens)), m.dashProjectTokens, m.dashLifetimeTokens, inner),
		"",
	)
	for _, line := range tableLines {
		content = append(content, "   "+line+"   ")
	}
	if len(m.dashProjectConversations) > 0 {
		content = append(content, "", "   Notes by conversation   ")
		for _, summary := range m.dashProjectConversations {
			content = append(content, fmt.Sprintf("   %s  %d notes · %d pending   ",
				summary.ID, summary.Notes, summary.Pending))
		}
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

// dashFailureBucketLimit bounds the buckets the dashboard asks for. The widget
// renders ten kinds at most, and a bucket is per (kind × tool × client build),
// so this leaves generous room to collapse from without letting a client that
// varies its version string grow the query without bound.
const dashFailureBucketLimit = 200

// dashFailureRow is one line of the failure widget: a kind and its counts.
type dashFailureRow struct {
	label     string
	calls     int64
	retryable int64
}

// failuresByKind collapses the (kind × tool × client) buckets FailureSummary
// returns down to the kind alone.
//
// Kind is the one dimension the dashboard does not already show: "Top Tools
// (uptime)" carries a per-tool Errors column, so repeating the tool here would
// restate it while crowding out the fact that is new — WHAT the failures were.
// The tool and client breakdown stays one command away in `plumb stats
// --failures`, where there is room for it.
//
// Ordering is busiest-first, ties broken by label, so the widget does not
// reshuffle between polls.
func failuresByKind(buckets []stats.FailureCount) []dashFailureRow {
	byLabel := make(map[string]dashFailureRow, len(buckets))
	for _, b := range buckets {
		row := byLabel[b.Label()]
		row.label = b.Label()
		row.calls += b.Calls
		row.retryable += b.Retryable
		byLabel[b.Label()] = row
	}
	out := make([]dashFailureRow, 0, len(byLabel))
	for _, row := range byLabel {
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b dashFailureRow) int {
		if a.calls != b.calls {
			return cmp.Compare(b.calls, a.calls)
		}
		return cmp.Compare(a.label, b.label)
	})
	return out
}

// dashFailuresWidget renders uptime failures grouped by kind, or nil when this
// daemon run has recorded none — an empty box would claim shelf space to say
// nothing, and "no failures" is already the dashboard's resting state.
func (m Model) dashFailuresWidget(width int) []string {
	rows := failuresByKind(m.dashUptimeFailures)
	if len(rows) == 0 {
		return nil
	}
	inner := max(width-2, 40)
	content := dashFailuresTable(max(inner-6, 20), rows)
	for i, line := range content {
		content[i] = "   " + line + "   "
	}
	return dashBox(" Failures by Kind (uptime) ", inner, content)
}

func dashFailuresTable(width int, rows []dashFailureRow) []string {
	const (
		callsW     = 9
		retryableW = 12
	)
	kindW := max(width-callsW-retryableW, 12)
	// Below 33 cells the kind clamp wins and the rows are wider than the width
	// asked for. The rule stretches to the rows rather than the request, so the
	// separator never comes up short under a narrow terminal.
	rowW := max(width, kindW+callsW+retryableW)
	if len(rows) > 10 {
		rows = rows[:10]
	}

	header := HintStyle.Width(kindW).Render("Kind") +
		HintStyle.Width(callsW).Align(lipgloss.Right).Render("Calls") +
		HintStyle.Width(retryableW).Align(lipgloss.Right).Render("Retryable")
	content := make([]string, 0, len(rows)+2)
	content = append(content, header, SepStyle.Render(strings.Repeat("╌", rowW)))
	for _, r := range rows {
		retryable := MutedStyle.Render("—")
		if r.retryable > 0 {
			retryable = DetailStyle.Render(formatLargeInt(r.retryable))
		}
		content = append(content,
			WarnStyle.Width(kindW).Render(textfmt.Ellipsis(r.label, kindW-1))+
				DetailStyle.Width(callsW).Align(lipgloss.Right).Render(formatLargeInt(r.calls))+
				lipgloss.NewStyle().Width(retryableW).Align(lipgloss.Right).Render(retryable))
	}
	return content
}
