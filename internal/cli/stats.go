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
	PrintLogo()

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
		printCLIDiagnostic(os.Stdout, cliDiagnostic{
			Kind:  "info",
			Title: "No statistics recorded yet",
			Body:  fmt.Sprintf("No statistics recorded yet for %s. Stats live at <workspace>/.plumb/stats.db — make some tool calls first.", contractSessionPath(ws)),
		})
		return nil
	}
	defer db.Close()

	filter := stats.Filter{}

	total := db.TotalCalls(filter)
	if total == 0 {
		printCLIDiagnostic(os.Stdout, cliDiagnostic{
			Kind:  "info",
			Title: "No statistics recorded yet",
			Body:  fmt.Sprintf("No statistics for workspace %s.", contractSessionPath(ws)),
		})
		return nil
	}

	tui.RebuildStyles()

	saved := db.TotalTokensSaved(filter)

	// Structured Context Block
	ctxBox := lipgloss.NewStyle().
		Border(ContextBorder, false, false, false, true).
		BorderForeground(tui.SepStyle.GetForeground()).
		PaddingLeft(1).
		Render(fmt.Sprintf("%s\n%s",
			contractSessionPath(ws),
			tui.MutedStyle.Render(fmt.Sprintf("↳ %d total calls · ~%s tokens saved", total, stats.FormatSavings(int(saved)))),
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
		Border(DottedBorder).
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

	const (
		wSess   = 10 // 8 hex chars + 2 padding
		wStatus = 8  // "Status" (6) padded to 8; ✓/✗ centred within
		wMs     = 3  // duration digits min width
	)
	wWhen := 8 // "When"
	wTool := 4 // "Tool"
	for _, c := range recent {
		if l := len(humanAge(c.CalledAt)); l > wWhen {
			wWhen = l
		}
		if l := len(c.Tool); l > wTool {
			wTool = l
		}
	}

	wWhen += 2
	wTool += 2

	// wMs + "  " + wStatus + "  " + wSess
	headerWidth := wWhen + wTool + wMs + 2 + wStatus + 2 + wSess
	fmt.Println(tui.SepStyle.Render(strings.Repeat("╌", headerWidth)))
	fmt.Printf("%s%s%s  %s  %s\n",
		padRight(tui.HintStyle.Render("When"), wWhen),
		padRight(tui.HintStyle.Render("Tool"), wTool),
		padRight(tui.HintStyle.Render("ms"), wMs),
		padRight(tui.HintStyle.Render("Status"), wStatus),
		tui.HintStyle.Render("Session"),
	)
	fmt.Println(tui.SepStyle.Render(strings.Repeat("╌", headerWidth)))

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
		sess := shortSessionID(c.SessionID)

		when   := padRight(humanAge(c.CalledAt), wWhen)
		tool   := padRight(c.Tool, wTool)
		ms     := padRight(fmt.Sprintf("%d", c.DurationMs), wMs)
		status := centerStr(ok, wStatus)

		if !c.Success {
			fmt.Println(tui.WarnStyle.Render(when+tool+ms) + "  " + status + "  " + tui.MutedStyle.Render(sess))
		} else {
			fmt.Println(when + tool + ms + "  " + status + "  " + tui.MutedStyle.Render(sess))
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

// shortSessionID returns the first 8 characters of a session ID for compact display.
func shortSessionID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// padRight pads a string with spaces on the right up to a given visual width (ignoring ANSI codes).
func padRight(s string, width int) string {
	vis := lipgloss.Width(s)
	if vis >= width {
		return s
	}
	return s + strings.Repeat(" ", width-vis)
}

// centerStr centres a string within the given visual width (ignoring ANSI codes).
func centerStr(s string, width int) string {
	vis := lipgloss.Width(s)
	if vis >= width {
		return s
	}
	left := (width - vis) / 2
	right := width - vis - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
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
