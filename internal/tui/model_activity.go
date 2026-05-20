package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/golimpio/plumb/internal/monitor"
	"github.com/golimpio/plumb/internal/stats"
)

func (m Model) renderTopMenu(width int, dimmed bool) []string {
	selector := m.renderSectionSelector(dimmed)
	activityBox := m.renderActivityBox(dimmed)
	daemonBox := m.renderDaemonMetricsBox(dimmed)
	tokenBox := m.renderTokenSavingsBox(dimmed)
	selectorWidth := lipgloss.Width(selector[0])
	activityBoxWidth := lipgloss.Width(activityBox[0])
	daemonBoxWidth := lipgloss.Width(daemonBox[0])
	showDaemonBox := width >= selectorWidth+1+daemonBoxWidth+1+activityBoxWidth
	currentWidth := selectorWidth + 1 + activityBoxWidth
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
		pad := max(width-lipgloss.Width(line), 0)
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
		barWidth   = innerWidth - 2
	)

	value := "n/a"
	percent := 0.0
	if m.daemonMetricsOK && m.daemonMetrics.CPUAvailable {
		percent = clampPercent(m.daemonMetrics.CPUPercent)
		value = monitor.FormatCPU(percent)
	}
	titleText := " Daemon CPU (" + value + ") "
	topFill := max(boxWidth-lipgloss.Width("╭─")-lipgloss.Width(titleText)-lipgloss.Width("╮"), 0)

	filledPart, unfilledPart := percentSegmentBar(percent, barWidth)
	content := " " + sparkStyle.Render(filledPart) + border.Render(unfilledPart) + " "

	return []string{
		border.Render("╭─") + title.Render(titleText) + border.Render(strings.Repeat("─", topFill)+"╮"),
		border.Render("│") + content + border.Render("│"),
		border.Render("╰" + strings.Repeat("─", innerWidth) + "╯"),
	}
}

func (m Model) renderSectionSelector(dimmed bool) []string {
	border := SepStyle
	title := PanelHeaderFadedStyle
	textStyle := SelectedStyle
	hintStyle := MutedStyle
	if dimmed {
		border = SepInactiveStyle
		title = PanelHeaderInactiveStyle
		textStyle = InactiveStyle
		hintStyle = InactiveStyle
	}

	titleText := " Section "
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
	pad := max(sectionMenuWidth-2-lipgloss.Width(content)-lipgloss.Width(arrow)-1, 1)
	row := content + strings.Repeat(" ", pad) + arrow + " "
	topFill := max(sectionMenuWidth-lipgloss.Width("╭─")-lipgloss.Width(titleText)-lipgloss.Width("╮"), 0)

	return []string{
		border.Render("╭─") + title.Render(titleText) + border.Render(strings.Repeat("─", topFill)+"╮"),
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
		barWidth = 12
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
	topFill := max(innerWidth-lipgloss.Width("─")-lipgloss.Width(titleText), 0)

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

func formatUptimePrecise(d time.Duration) string {
	if d < time.Minute {
		return "< 1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm+", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh+", int(d.Hours()))
	}

	days := int(d.Hours() / 24)
	if days < 30 {
		return fmt.Sprintf("%dd+", days)
	}

	months := days / 30
	days %= 30
	if months < 12 {
		if days > 0 {
			return fmt.Sprintf("%dmo %dd+", months, days)
		}
		return fmt.Sprintf("%dmo+", months)
	}

	years := months / 12
	months %= 12
	if months > 0 {
		return fmt.Sprintf("%dy %dmo+", years, months)
	}
	return fmt.Sprintf("%dy+", years)
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
		sparkWidth = 16
	)

	windowStr := "1m"
	if m.activity.Window > 0 {
		windowStr = formatUptime(m.activity.Window)
	}
	titleText := fmt.Sprintf(" Activity (%s) ", windowStr)

	count := formatActivityCount(m.activity.Calls)
	countWidth := lipgloss.Width(count)
	spark := activitySparkline(m.activity.Buckets, sparkWidth)
	content := " " + renderActivityGraph(spark, sparkStyle, border) + " " + countStyle.Render(count) + " "
	innerWidth := lipgloss.Width(content)
	minInnerWidth := lipgloss.Width("─") + lipgloss.Width(titleText)
	if innerWidth < minInnerWidth {
		innerWidth = minInnerWidth
		content = " " + renderActivityGraph(spark, sparkStyle, border) + strings.Repeat(" ", max(innerWidth-lipgloss.Width(spark)-countWidth-2, 1)) + countStyle.Render(count) + " "
	}
	topFill := max(innerWidth-lipgloss.Width("─")-lipgloss.Width(titleText), 0)

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
	var ceiling int64 = 10 // Enforce a minimum ceiling so 1-2 calls don't draw a full 100% bar
	for _, v := range buckets {
		if v > ceiling {
			ceiling = v
		}
	}
	levels := []rune{' ', '⡀', '⡄', '⡆', '⡇', '⣇', '⣧', '⣷', '⣿'}
	for i := range width {
		bucketIdx := i * len(buckets) / width
		v := buckets[bucketIdx]
		if ceiling == 0 || v == 0 {
			out[i] = ' '
			continue
		}
		ratio := float64(v) / float64(ceiling)
		levelIdx := min(max(int(math.Ceil(ratio*float64(len(levels)-1))), 0), len(levels)-1)
		out[i] = levels[levelIdx]
	}
	return string(out)
}

func renderActivityGraph(spark string, active, inactive lipgloss.Style) string {
	var b strings.Builder
	for _, r := range spark {
		if r == ' ' {
			b.WriteString(inactive.Render("⣀"))
		} else {
			b.WriteString(active.Render(string(r)))
		}
	}
	return b.String()
}

func cpuSparkline(samples []float64, width int) string {
	if width <= 0 {
		return ""
	}
	if len(samples) == 0 {
		return strings.Repeat(" ", width)
	}
	out := make([]rune, width)
	levels := []rune{' ', '⡀', '⡄', '⡆', '⡇', '⣇', '⣧', '⣷', '⣿'}
	for i := range width {
		sampleIdx := i * len(samples) / width
		v := clampPercent(samples[sampleIdx])
		if v == 0 {
			out[i] = ' '
			continue
		}
		ratio := v / 100.0
		levelIdx := min(max(int(math.Ceil(ratio*float64(len(levels)-1))), 0), len(levels)-1)
		out[i] = levels[levelIdx]
	}
	return string(out)
}

func percentSegmentBar(percent float64, width int) (string, string) {
	if width <= 0 {
		return "", ""
	}
	percent = clampPercent(percent)
	filledBlocks := int((percent / 100.0) * float64(width))
	if percent > 0 && filledBlocks == 0 {
		filledBlocks = 1
	}
	filledBlocks = min(filledBlocks, width)
	return strings.Repeat("■", filledBlocks), strings.Repeat("■", width-filledBlocks)
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
	ratio := float64(tokens) / float64(targetTokens)
	if ratio > 1 {
		ratio = 1
	}
	if ratio < 0 {
		ratio = 0
	}

	filledBlocks := int(ratio * float64(width))

	if tokens > 0 && filledBlocks == 0 {
		filledBlocks = 1 // Show at least one block
	}
	filledBlocks = min(filledBlocks, width)

	filledStr := strings.Repeat("■", filledBlocks)
	unfilledLen := max(width-lipgloss.Width(filledStr), 0)

	return filledStr, strings.Repeat("■", unfilledLen)
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

func formatActivityCount(n int64) string {
	switch {
	case n >= 1_000_000:
		val := float64(n) / 1_000_000
		if val == float64(int64(val)) {
			return fmt.Sprintf("%.0fm", val)
		}
		return fmt.Sprintf("%.1fm", val)
	case n >= 1000:
		val := float64(n) / 1000
		if val >= 100 || val == float64(int64(val)) {
			return fmt.Sprintf("%.0fk", val)
		}
		return fmt.Sprintf("%.1fk", val)
	default:
		return fmt.Sprintf("%d", n)
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

func formatActiveSessionCount(n int64) string {
	switch n {
	case 0:
		return "no active sessions"
	case 1:
		return "1 active session"
	default:
		return fmt.Sprintf("%d active sessions", n)
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
