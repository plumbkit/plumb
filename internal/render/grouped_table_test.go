package render_test

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/render"
)

// newPlainTable builds a table whose border and header styles carry no colour,
// so the expected strings below contain no ANSI codes — a zero style renders
// text unchanged.
func newPlainTable(headers ...string) *render.GroupedTable {
	return render.NewGroupedTable(lipgloss.NewStyle(), lipgloss.NewStyle(), headers...)
}

func TestGroupedTableRender(t *testing.T) {
	rule := func(w int) string { return strings.Repeat("╌", w) }
	cases := []struct {
		name  string
		table func() *render.GroupedTable
		want  string
	}{
		{
			name: "single group has no inner rules",
			table: func() *render.GroupedTable {
				return newPlainTable("Tool", "Calls").
					Row("read_file", "12").
					Row("git", "3")
			},
			// Widths are global: "read_file" widens Tool to 9, so the rule
			// spans 9+5+2 = 16 and "git" pads to the same column.
			want: strings.Join([]string{
				rule(16),
				"Tool" + strings.Repeat(" ", 7) + "Calls",
				rule(16),
				"read_file  12",
				"git" + strings.Repeat(" ", 8) + "3",
			}, "\n"),
		},
		{
			name: "groups separated by a rule, columns aligned across groups",
			table: func() *render.GroupedTable {
				return newPlainTable("Name", "Status").
					Row("alpha", "registered").
					Row("beta", "missing").
					NextGroup().
					Row("gamma-long", "stale (installed by 0.15.1)")
			},
			// "gamma-long" sits in the second group yet widens the Name
			// column to 10 for the first group's rows; the suffixed stale
			// status widens Status to 27, giving a 10+27+2 = 39 rule.
			want: strings.Join([]string{
				rule(39),
				"Name" + strings.Repeat(" ", 8) + "Status",
				rule(39),
				"alpha" + strings.Repeat(" ", 7) + "registered",
				"beta" + strings.Repeat(" ", 8) + "missing",
				rule(39),
				"gamma-long  stale (installed by 0.15.1)",
			}, "\n"),
		},
		{
			name: "ansi cells are measured by visible width",
			table: func() *render.GroupedTable {
				return newPlainTable("Mark", "Value").
					Row("\x1b[31mred\x1b[0m", "ok")
			},
			// The escape codes cost nothing: the cell measures 3 wide, pads
			// to Mark's 4, then takes the two-space gap.
			want: strings.Join([]string{
				rule(11),
				"Mark  Value",
				rule(11),
				"\x1b[31mred\x1b[0m   ok",
			}, "\n"),
		},
		{
			name: "zero-row group is skipped",
			table: func() *render.GroupedTable {
				return newPlainTable("K", "V").
					Row("a", "1").
					NextGroup() // trailing group with no rows: no trailing rule
			},
			want: strings.Join([]string{
				rule(4),
				"K  V",
				rule(4),
				"a  1",
			}, "\n"),
		},
		{
			name: "zero-row group between live groups adds no extra rule",
			table: func() *render.GroupedTable {
				return newPlainTable("K", "V").
					Row("a", "1").
					NextGroup().
					NextGroup().
					Row("b", "2")
			},
			want: strings.Join([]string{
				rule(4),
				"K  V",
				rule(4),
				"a  1",
				rule(4),
				"b  2",
			}, "\n"),
		},
		{
			name:  "no rows renders rules and header only",
			table: func() *render.GroupedTable { return newPlainTable("A", "B") },
			want: strings.Join([]string{
				rule(4),
				"A  B",
				rule(4),
			}, "\n"),
		},
		{
			name: "mismatched cell counts degrade instead of panicking",
			table: func() *render.GroupedTable {
				return newPlainTable("A", "B").
					Row("only").       // missing cell renders empty
					Row("x", "y", "z") // extra cell is dropped
			},
			want: strings.Join([]string{
				rule(7), // "only" widens A to 4: 4+1+2
				"A" + strings.Repeat(" ", 5) + "B",
				rule(7),
				"only  ",
				"x" + strings.Repeat(" ", 5) + "y",
			}, "\n"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.table().Render(); got != tc.want {
				t.Errorf("Render():\ngot:  %q\nwant: %q", got, tc.want)
			}
		})
	}
}
