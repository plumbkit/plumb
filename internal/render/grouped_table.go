package render

import (
	"strings"

	"charm.land/lipgloss/v2"
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
	// groups[g][r] is row r of group g — itself one cell per column.
	groups [][][]string
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

// joinRow lays out one row: every cell styled with style, a two-space gap
// between columns, each column padded to its width except the last (no
// trailing spaces). Cells beyond the column count are dropped and missing
// cells render empty, so a malformed Row degrades instead of panicking.
func joinRow(cells []string, widths []int, style lipgloss.Style) string {
	var b strings.Builder
	for i := range widths {
		if i > 0 {
			b.WriteString("  ")
		}
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		cell = style.Render(cell)
		if i < len(widths)-1 {
			cell = PadRight(cell, widths[i])
		}
		b.WriteString(cell)
	}
	return b.String()
}
