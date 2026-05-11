package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/stats"
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

	// Tool summary table
	summary, err := db.Summary(filter)
	if err != nil {
		return fmt.Errorf("querying summary: %w", err)
	}

	fmt.Println("Tool Call Summary")
	fmt.Println(strings.Repeat("─", 78))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TOOL\tCALLS\tAVG ms\tP95 ms\tINPUT\tOUTPUT\tERRORS\tSAVED")
	for _, s := range summary {
		savedStr := "—"
		if s.TokensSaved > 0 {
			savedStr = "~" + stats.FormatSavings(int(s.TokensSaved)) + " tok"
		}
		fmt.Fprintf(w, "%s\t%d\t%.0f\t%d\t%.1f KB\t%.1f KB\t%d\t%s\n",
			s.Tool, s.Calls, s.AvgMs, s.P95Ms,
			s.TotalInputKB, s.TotalOutputKB, s.Errors, savedStr,
		)
	}
	_ = w.Flush()

	// Recent calls
	recent, err := db.Recent(statsFlagLimit, filter)
	if err != nil {
		return fmt.Errorf("querying recent calls: %w", err)
	}

	fmt.Printf("\nRecent Calls (last %d)\n", statsFlagLimit)
	fmt.Println(strings.Repeat("─", 78))

	wr := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(wr, "WHEN\tTOOL\tWORKSPACE\tms\tOK")
	for _, c := range recent {
		ok := "✓"
		if !c.Success {
			ok = "✗"
		}
		fmt.Fprintf(wr, "%s\t%s\t%s\t%d\t%s\n",
			humanAge(c.CalledAt),
			c.Tool,
			contractSessionPath(c.Workspace),
			c.DurationMs,
			ok,
		)
		if !c.Success && c.ErrorMsg != "" {
			fmt.Fprintf(wr, "  ↳ %s\n", truncateErr(c.ErrorMsg, 200))
		}
	}
	_ = wr.Flush()

	return nil
}

// truncateErr collapses newlines and trims long error messages so the
// stats table's continuation line stays readable.
func truncateErr(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
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
