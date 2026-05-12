package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/stats"
	"github.com/golimpio/plumb/internal/tui"
)

var (
	statsFlagWorkspace string
	statsFlagLimit     int
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show tool call statistics",
	RunE:  runStats,
}

func init() {
	statsCmd.Flags().StringVar(&statsFlagWorkspace, "workspace", "", "workspace path to inspect (defaults to current directory)")
	statsCmd.Flags().IntVar(&statsFlagLimit, "limit", 20, "number of recent calls to show")
}

func runStats(_ *cobra.Command, _ []string) error {
	ws := statsFlagWorkspace
	if ws == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
		ws = cwd
	}

	db, err := stats.OpenReadOnly(stats.DBPathFor(ws))
	if err != nil {
		return fmt.Errorf("opening stats db: %w", err)
	}
	if db == nil {
		fmt.Printf("No statistics recorded yet for %s.\n", contractSessionPath(ws))
		fmt.Println("Stats live at <workspace>/.plumb/stats.db — make some tool calls first.")
		return nil
	}
	defer db.Close()

	filter := stats.Filter{}

	total := db.TotalCalls(filter)
	if total == 0 {
		if ws != "" {
			fmt.Printf("No statistics for workspace %q.\n", ws)
		} else {
			fmt.Println("No statistics recorded yet.")
		}
		return nil
	}

	saved := db.TotalTokensSaved(filter)
	fmt.Printf("plumb stats — %s\n", contractSessionPath(ws))
	fmt.Printf("  %d total calls  ·  ~%s tokens saved (estimate)\n\n",
		total, stats.FormatSavings(int(saved)))

	tui.RebuildStyles()

	// Tool summary table
	summary, err := db.Summary(filter)
	if err != nil {
		return fmt.Errorf("querying summary: %w", err)
	}

	fmt.Println("Tool Call Summary")

	t1 := table.New().
		Border(lipgloss.NormalBorder()).
		BorderRow(false).
		BorderColumn(false).
		BorderLeft(false).
		BorderRight(false).
		BorderTop(true).
		BorderBottom(false).
		BorderStyle(tui.SepStyle).
		Headers("TOOL", "CALLS", "AVG ms", "P95 ms", "INPUT", "OUTPUT", "ERRORS", "SAVED").
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().PaddingRight(2)
			if row == table.HeaderRow {
				return s.Inherit(tui.HintStyle)
			}
			return s
		})

	for _, s := range summary {
		savedStr := "—"
		if s.TokensSaved > 0 {
			savedStr = "~" + stats.FormatSavings(int(s.TokensSaved)) + " tok"
		}
		
		t1.Row(
			s.Tool,
			fmt.Sprintf("%d", s.Calls),
			fmt.Sprintf("%.0f", s.AvgMs),
			fmt.Sprintf("%d", s.P95Ms),
			fmt.Sprintf("%.1f KB", s.TotalInputKB),
			fmt.Sprintf("%.1f KB", s.TotalOutputKB),
			fmt.Sprintf("%d", s.Errors),
			savedStr,
		)
	}
	fmt.Println(t1.Render())

	// Recent calls
	recent, err := db.Recent(statsFlagLimit, filter)
	if err != nil {
		return fmt.Errorf("querying recent calls: %w", err)
	}

	fmt.Printf("\nRecent Calls (last %d)\n", statsFlagLimit)

	t2 := table.New().
		Border(lipgloss.NormalBorder()).
		BorderRow(false).
		BorderColumn(false).
		BorderLeft(false).
		BorderRight(false).
		BorderTop(true).
		BorderBottom(false).
		BorderStyle(tui.SepStyle).
		Headers("WHEN", "TOOL", "WORKSPACE", "ms", "OK").
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().PaddingRight(2)
			if row == table.HeaderRow {
				return s.Inherit(tui.HintStyle)
			}
			return s
		})

	for _, c := range recent {
		ok := tui.OkStyle.Render("✓")
		if !c.Success {
			ok = tui.WarnStyle.Render("✗")
		}
		
		// If there's an error message, we append it to the Tool column using
		// newlines and indentation. The lipgloss table component supports
		// multiline cells out of the box.
		toolCell := c.Tool
		if !c.Success && c.ErrorMsg != "" {
			var errBuf strings.Builder
			errBuf.WriteString(c.Tool)
			for i, line := range strings.Split(c.ErrorMsg, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				prefix := "  "
				if i == 0 {
					prefix = "↳ "
				}
				errBuf.WriteString("\n" + tui.WarnStyle.Render(prefix+line))
			}
			toolCell = errBuf.String()
		}

		t2.Row(
			humanAge(c.CalledAt),
			toolCell,
			contractSessionPath(c.Workspace),
			fmt.Sprintf("%d", c.DurationMs),
			ok,
		)
	}
	fmt.Println(t2.Render())

	return nil
}

// humanAge formats a past time as a human-readable age string.
func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02")
	}
}
