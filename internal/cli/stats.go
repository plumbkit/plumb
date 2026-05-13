package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/reflow/wordwrap"
	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/stats"
	"github.com/golimpio/plumb/internal/tui"
)

var (
	statsFlagWorkspace string
	statsFlagLimit     int
)

var statsCmd = &cobra.Command{
	Use:     "stats",
	Aliases: []string{"status"},
	Short:   "Show tool call statistics",
	RunE:    runStats,
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

	tui.RebuildStyles()

	// Print Logo
	PrintLogo()

	saved := db.TotalTokensSaved(filter)
	
	// Structured Context Block
	ctxBox := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, false, false, true).
		BorderForeground(tui.ActiveTheme.Accent).
		PaddingLeft(1).
		Render(fmt.Sprintf("%s\n%s",
			contractSessionPath(ws),
			tui.MutedStyle.Render(fmt.Sprintf("%d total calls · ~%s tokens saved", total, stats.FormatSavings(int(saved)))),
		))
	fmt.Println(ctxBox)
	fmt.Println()

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
		Headers("Tool", "Calls", "Avg ms", "P95 ms", "Input", "Output", "Errors", "Saved").
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

	wWhen := 8 // "WHEN"
	wTool := 4 // "TOOL"
	wWork := 9 // "WORKSPACE"
	for _, c := range recent {
		if l := len(humanAge(c.CalledAt)); l > wWhen {
			wWhen = l
		}
		if l := len(c.Tool); l > wTool {
			wTool = l
		}
		if l := len(contractSessionPath(c.Workspace)); l > wWork {
			wWork = l
		}
	}

	wWhen += 2
	wTool += 2
	wWork += 2

	// Print header
	// 11 = "ms" (3 chars, %-3s) + 2 spaces padding + "Status" (6 chars)
	headerWidth := wWhen + wTool + wWork + 11
	fmt.Println(tui.SepStyle.Render(strings.Repeat("─", headerWidth)))
	fmt.Printf("%s%s%s%-3s  %s\n",
		padRight(tui.HintStyle.Render("When"), wWhen),
		padRight(tui.HintStyle.Render("Tool"), wTool),
		padRight(tui.HintStyle.Render("Workspace"), wWork),
		tui.HintStyle.Render("ms"),
		tui.HintStyle.Render("Status"),
	)
	fmt.Println(tui.SepStyle.Render(strings.Repeat("─", headerWidth)))

	// Calculate terminal width for error wrapping
	termWidth := 80
	if w, _, err := term.GetSize(uintptr(os.Stdout.Fd())); err == nil && w > 0 {
		termWidth = w
	}
	errMaxWidth := termWidth - wWhen - 2
	if errMaxWidth < 40 {
		errMaxWidth = 40
	}

	for _, c := range recent {
		ok := tui.OkStyle.Render("✓")
		if !c.Success {
			ok = tui.WarnStyle.Render("✗")
		}

		// %-3d (3 chars) + "  " (2 spaces) = 5.
		// The ok character aligns perfectly with the 'S' in "Status".
		rowStr := fmt.Sprintf("%s%s%s%-3d  %s",
			padRight(humanAge(c.CalledAt), wWhen),
			padRight(c.Tool, wTool),
			padRight(contractSessionPath(c.Workspace), wWork),
			c.DurationMs,
			ok,
		)

		if !c.Success {
			// Print the entire row in WarnStyle, but preserve the spacing layout
			// by removing the OK string and replacing it with the colored one later
			rawRow := fmt.Sprintf("%s%s%s%-3d  ",
				padRight(humanAge(c.CalledAt), wWhen),
				padRight(c.Tool, wTool),
				padRight(contractSessionPath(c.Workspace), wWork),
				c.DurationMs,
			)
			fmt.Println(tui.WarnStyle.Render(rawRow) + ok)
		} else {
			fmt.Println(rowStr)
		}

		if !c.Success && c.ErrorMsg != "" {
			lines := strings.Split(c.ErrorMsg, "\n")
			for i, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				
				// Apply wordwrap to long lines
				wrapped := wordwrap.String(line, errMaxWidth)
				wrappedLines := strings.Split(wrapped, "\n")
				
				for j, wl := range wrappedLines {
					prefix := "  "
					// Only use the arrow for the very first line of the entire error message
					if i == 0 && j == 0 {
						prefix = "↳ "
					}
					fmt.Printf("%*s%s\n", wWhen, "", tui.WarnStyle.Render(prefix+wl))
				}
			}
		}
	}

	return nil
}

// padRight pads a string with spaces up to a given visual width (ignoring ANSI codes).
func padRight(s string, width int) string {
	vis := lipgloss.Width(s)
	if vis >= width {
		return s
	}
	return s + strings.Repeat(" ", width-vis)
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

