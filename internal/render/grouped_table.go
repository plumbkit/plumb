package render

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// GroupedTable is a hand-rendered table whose rows are partitioned into groups
// separated by full-width dotted rules — the one layout lipgloss's table
// package cannot express, because its borders wrap the whole table rather than
// slicing between row groups. Column widths are computed globally across all
// groups and the header, so columns stay aligned across the separators.
type GroupedTable struct {
	borderStyle lipgloss.Style
	headerStyle lipgloss.Style
	headers     []string
	maxWidth    int
	// groups[g][r] is row r of group g — itself one cell per column.
	groups [][][]string
}

// MaxWidth limits the rendered table to width columns. Cells that do not fit
// wrap within their column; a non-positive width keeps natural column widths.
func (t *GroupedTable) MaxWidth(width int) *GroupedTable {
	t.maxWidth = width
	return t
}

// NewGroupedTable returns a table with the given header cells, rendered with
// headerStyle; separator rules are rendered with borderStyle.
func NewGroupedTable(borderStyle, headerStyle lipgloss.Style, headers ...string) *GroupedTable {
	return &GroupedTable{
		borderStyle: borderStyle,
		headerStyle: headerStyle,
		headers:     headers,
		groups:      [][][]string{nil},
	}
}

// Row appends a row to the current group.
func (t *GroupedTable) Row(cells ...string) *GroupedTable {
	last := len(t.groups) - 1
	t.groups[last] = append(t.groups[last], cells)
	return t
}

// NextGroup starts a new row group. Render draws a separator rule between
// groups; a group with no rows is skipped entirely.
func (t *GroupedTable) NextGroup() *GroupedTable {
	t.groups = append(t.groups, nil)
	return t
}

// Render draws the table: a top rule, the header line, a rule, then each
// non-empty group's rows with a rule between groups and no trailing rule.
func (t *GroupedTable) Render() string {
	cols := len(t.headers)
	widths := make([]int, cols)
	for i, h := range t.headers {
		widths[i] = lipgloss.Width(h)
	}
	for _, g := range t.groups {
		for _, row := range g {
			for i := 0; i < cols && i < len(row); i++ {
				if w := lipgloss.Width(row[i]); w > widths[i] {
					widths[i] = w
				}
			}
		}
	}
	fitColumnWidths(widths, t.headers, t.maxWidth)

	ruleWidth := 2 * (cols - 1)
	for _, w := range widths {
		ruleWidth += w
	}
	if ruleWidth < 0 {
		ruleWidth = 0
	}
	rule := t.borderStyle.Render(strings.Repeat("╌", ruleWidth))

	var b strings.Builder
	b.WriteString(rule)
	b.WriteByte('\n')
	b.WriteString(joinRow(t.headers, widths, t.headerStyle))
	b.WriteByte('\n')
	b.WriteString(rule)

	emitted := false
	for _, g := range t.groups {
		if len(g) == 0 {
			continue
		}
		if emitted {
			b.WriteByte('\n')
			b.WriteString(rule)
		}
		for _, row := range g {
			b.WriteByte('\n')
			b.WriteString(joinRow(row, widths, lipgloss.NewStyle()))
		}
		emitted = true
	}
	return b.String()
}

// fitColumnWidths gives long cells the remaining space after preserving header
// widths. The final detail column keeps up to 16 columns when space permits.
func fitColumnWidths(widths []int, headers []string, maxWidth int) {
	if maxWidth <= 0 || len(widths) == 0 {
		return
	}
	available := maxWidth - 2*(len(widths)-1)
	minimums := make([]int, len(widths))
	for i, h := range headers {
		minimums[i] = min(widths[i], max(1, lipgloss.Width(h)))
	}
	last := len(widths) - 1
	minimums[last] = min(widths[last], max(minimums[last], 16))
	shrinkColumns(widths, minimums, available)
	// At very narrow widths even the headers must wrap.
	for i := range minimums {
		minimums[i] = 1
	}
	shrinkColumns(widths, minimums, available)
}

func shrinkColumns(widths, minimums []int, available int) {
	total := 0
	for _, w := range widths {
		total += w
	}
	for total > available {
		widest, room := -1, 0
		for i, w := range widths {
			if w-minimums[i] > room {
				widest, room = i, w-minimums[i]
			}
		}
		if widest < 0 {
			return
		}
		cut := min(room, total-available)
		widths[widest] -= cut
		total -= cut
	}
}

// joinRow lays out one row: every cell styled with style, a two-space gap
// between columns, each column padded to its width except the last (no
// trailing spaces). Cells beyond the column count are dropped and missing
// cells render empty, so a malformed Row degrades instead of panicking.
func joinRow(cells []string, widths []int, style lipgloss.Style) string {
	lines := make([][]string, len(widths))
	height := 1
	for i, width := range widths {
		if i < len(cells) {
			lines[i] = strings.Split(ansi.Wrap(cells[i], width, ""), "\n")
		} else {
			lines[i] = []string{""}
		}
		height = max(height, len(lines[i]))
	}
	var b strings.Builder
	for line := range height {
		if line > 0 {
			b.WriteByte('\n')
		}
		for i := range widths {
			if i > 0 {
				b.WriteString("  ")
			}
			cell := ""
			if line < len(lines[i]) {
				cell = lines[i][line]
			}
			cell = style.Render(cell)
			if i < len(widths)-1 {
				cell = PadRight(cell, widths[i])
			}
			b.WriteString(cell)
		}
	}
	return b.String()
}
