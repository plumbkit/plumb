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
		dots := max(total-lipgloss.Width(p[0])-lipgloss.Width(p[1])-2, minLeaderDots)
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

// ContractHome is ContractPath's whole-string sibling: it replaces every
// occurrence of the home directory in s with ~, for text that embeds absolute
// paths rather than being one — an error message quoting the file it failed on,
// typically, which a caller wants to read the same way as the paths beside it.
func ContractHome(s string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return s
	}
	return strings.ReplaceAll(s, home, "~")
}

// ShortenPath abbreviates p to at most max display columns by replacing whole
// interior segments with a single …, keeping the root and as many trailing
// segments as fit — the end of a config path is what identifies it, and cutting
// on a separator keeps the result readable as a path rather than as a cut
// string. A path that already fits comes back untouched, and maxW <= 0 disables
// shortening entirely.
//
// It exists because a table column is sized to its widest cell: one deeply
// nested config path sets the width of every row beside it, so the cap has to
// be applied per cell before layout, not after.
func ShortenPath(p string, maxW int) string {
	if maxW <= 0 || lipgloss.Width(p) <= maxW {
		return p
	}
	// segs[0] is "~", "" for an absolute path, or a relative first segment —
	// either way it is the anchor a reader places the rest against, so it is
	// always kept. Widest-first means the result keeps as much context as fits.
	segs := strings.Split(p, "/")
	for keep := len(segs) - 2; keep >= 1; keep-- {
		candidate := segs[0] + "/…/" + strings.Join(segs[len(segs)-keep:], "/")
		if lipgloss.Width(candidate) <= maxW {
			return candidate
		}
	}
	return TruncatePathLeft(p, maxW)
}

// TruncatePathLeft keeps the rightmost maxW runes of p, prefixed with …. It is
// the opposite end from textfmt.Ellipsis on purpose: the discriminating part of
// a path is its tail, so a display cut has to come off the front.
//
// ShortenPath falls back to it when no segment boundary yields a short enough
// path — a single very long filename, or a path with no separator to cut on —
// and the TUI's own path-style strategies use it directly.
func TruncatePathLeft(p string, maxW int) string {
	r := []rune(p)
	if len(r) <= maxW {
		return p
	}
	if maxW <= 1 {
		return "…"
	}
	return "…" + string(r[len(r)-(maxW-1):])
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
