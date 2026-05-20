package tui

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

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

	graphLines := m.dashActivityGraphLines(chartW)
	out := make([]string, 0, 4+len(graphLines))
	out = append(out,
		dashActivityBorder("╭", "╮", inner, topL, topR),
		SepStyle.Render("│")+strings.Repeat(" ", inner)+SepStyle.Render("│"),
	)
	for _, line := range graphLines {
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
	const halfH = 2

	// Braille bottom-fill patterns: left/right column filled upward.
	botL := [5]int{0, 0x40, 0x44, 0x46, 0x47}
	botR := [5]int{0, 0x80, 0xA0, 0xB0, 0xB8}

	// Braille top-fill patterns: left/right column filled downward.
	topL := [5]int{0, 0x01, 0x03, 0x07, 0x47}
	topR := [5]int{0, 0x08, 0x18, 0x38, 0xB8}

	gridLife := dashBuildGrid(m.dashLifetimeBuckets, false, width, halfH, botL, botR, topL, topR)
	gridDaem := dashBuildGrid(m.dashDaemBuckets, true, width, halfH, botL, botR, topL, topR)

	lines := make([]string, 0, halfH*2)

	// Top half: lifetime data, bottom-fill.
	// Only the innermost row (r == halfH-1) shows ⣀ when idle; the outer row is blank.
	for r := range halfH {
		bg := rune(' ')
		if r == halfH-1 {
			bg = '⣀'
		}
		lines = append(lines, dashRenderRow(gridLife[r], bg, width))
	}

	// Bottom half: daemon data, top-fill.
	// Only the innermost row (r == 0) shows ⠉ when idle; the outer row is blank.
	for r := range halfH {
		bg := rune(' ')
		if r == 0 {
			bg = '⠉'
		}
		lines = append(lines, dashRenderRow(gridDaem[r], bg, width))
	}

	return lines
}

// dashBuildGrid converts a slice of call counts into a halfH×width braille pixel grid.
// fillDown=true fills from the top (daemon style); fillDown=false fills from the bottom (lifetime style).
func dashBuildGrid(buckets []int64, fillDown bool, width, halfH int, botL, botR, topL, topR [5]int) [][]int {
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

// dashRenderRow converts one row of braille pixel codes into a styled string.
func dashRenderRow(row []int, bgRune rune, width int) string {
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
				run.WriteRune(rune(0x2800 + row[k])) //nolint:gosec // G115: row[k] is a braille pixel code in [0,255]; 0x2800+255=0x28FF is within rune range
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
