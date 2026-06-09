// Package render provides shared, pure presentation helpers for CLI and TUI output.
// It is leaf-level: it imports only standard library and external rendering libraries —
// never internal domain or transport packages.
package render

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
)

// leaderDot is the dotted-leader glyph shared by the TUI memory rows and the
// `plumb debug mem` CLI rows.
const leaderDot = "⣀"

// minLeaderDots is the smallest leader run rendered, so even the widest
// label/value pair keeps a visible gap.
const minLeaderDots = 2

// LeaderRows aligns label/value pairs into dotted-leader rows — labels
// left-aligned, values right-aligned at a common right edge, dotted leaders
// filling the gap (e.g. "HeapAlloc ⣀⣀⣀⣀⣀⣀ 12 MB"). Pure text, no colour, so it
// is safe to pipe. Used by `plumb debug mem`.
func LeaderRows(pairs [][2]string) []string {
	maxLabel, maxValue := 0, 0
	for _, p := range pairs {
		if w := lipgloss.Width(p[0]); w > maxLabel {
			maxLabel = w
		}
		if w := lipgloss.Width(p[1]); w > maxValue {
			maxValue = w
		}
	}
	total := maxLabel + maxValue + minLeaderDots + 2 // +2 for the spaces around the leader
	rows := make([]string, 0, len(pairs))
	for _, p := range pairs {
		dots := total - lipgloss.Width(p[0]) - lipgloss.Width(p[1]) - 2
		if dots < minLeaderDots {
			dots = minLeaderDots
		}
		rows = append(rows, p[0]+" "+strings.Repeat(leaderDot, dots)+" "+p[1])
	}
	return rows
}

// contextBorder is a left-only border used for workspace context blocks.
var contextBorder = lipgloss.Border{Left: "┊"}

// dottedBorder is a fully dotted border used for CLI tables.
var dottedBorder = lipgloss.Border{
	Top:          "╌",
	Bottom:       "╌",
	Left:         "┊",
	Right:        "┊",
	TopLeft:      "╭",
	TopRight:     "╮",
	BottomLeft:   "╰",
	BottomRight:  "╯",
	Middle:       "┼",
	MiddleTop:    "┬",
	MiddleBottom: "┴",
	MiddleLeft:   "├",
	MiddleRight:  "┤",
}

// ContractPath replaces the home directory prefix in p with ~.
func ContractPath(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && strings.HasPrefix(p, home) {
		p = "~" + p[len(home):]
	}
	return p
}

// HumanAge formats a past time as a concise human-readable age string.
// Times within the last minute show seconds; within an hour show minutes;
// within a day show hours; older times show the date as "Jan 2".
func HumanAge(t time.Time) string {
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

// PadRight pads s with spaces on the right to the given visual width,
// using lipgloss.Width to measure so ANSI codes are not counted.
func PadRight(s string, width int) string {
	v := lipgloss.Width(s)
	if v >= width {
		return s
	}
	return s + strings.Repeat(" ", width-v)
}

// HumanBytes formats a byte count for one-line CLI/TUI output, using binary
// (MiB/KiB) units. The shared presentation helper for byte sizes; note the
// Intelligence-layer topology package keeps its own copy (status.formatBytes)
// because it must not import this presentation package (layering rule).
func HumanBytes(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// PadLeft pads s with spaces on the left to the given visual width,
// using lipgloss.Width to measure so ANSI codes are not counted.
func PadLeft(s string, width int) string {
	v := lipgloss.Width(s)
	if v >= width {
		return s
	}
	return strings.Repeat(" ", width-v) + s
}

// ContextBox renders content inside a left-bordered sidebar box.
// borderStyle provides the left-border foreground colour (GetForeground is called on it).
func ContextBox(content string, borderStyle lipgloss.Style) string {
	return lipgloss.NewStyle().
		Border(contextBorder, false, false, false, true).
		BorderForeground(borderStyle.GetForeground()).
		PaddingLeft(1).
		Render(content)
}

// DottedTableBase returns a new table.Table pre-configured with the shared dotted
// border, no row/column separators, and a StyleFunc that applies PaddingRight(2)
// to all cells and inherits headerStyle for the header row.
func DottedTableBase(borderStyle, headerStyle lipgloss.Style) *table.Table {
	return table.New().
		Border(dottedBorder).
		BorderRow(false).
		BorderColumn(false).
		BorderLeft(false).
		BorderRight(false).
		BorderTop(true).
		BorderBottom(false).
		BorderStyle(borderStyle).
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().PaddingRight(2)
			if row == table.HeaderRow {
				return s.Inherit(headerStyle)
			}
			return s
		})
}
