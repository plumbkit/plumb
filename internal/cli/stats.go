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

	"github.com/golimpio/plumb/internal/config"
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
		ws = "."
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	ws, err = resolveCLIWorkspace(ws, cfg)
	if err != nil {
		return err
	}

	db, err := stats.OpenReadOnly()
	if err != nil {
		return fmt.Errorf("opening stats db: %w", err)
	}
	if db == nil {
		printCLIDiagnostic(os.Stdout, cliDiagnostic{
			Kind:  "info",
			Title: "No statistics recorded yet",
			Body:  "No statistics recorded yet. Make some tool calls first.",
		})
		return nil
	}
	defer db.Close()

	// Filter stats to the requested workspace.
	filter := stats.Filter{Workspace: ws}

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
	fmt.Println("Tool Call Summary")
	summaryTable, err := statsToolSummaryTable(db, filter)
	if err != nil {
		return err
	}
	fmt.Println(summaryTable)

	// Recent calls
	recent, err := db.Recent(statsFlagLimit, filter)
	if err != nil {
		return fmt.Errorf("querying recent calls: %w", err)
	}

	fmt.Printf("\nRecent Calls (last %d)\n", statsFlagLimit)

	const (
		wSessID = 10 // 8 hex chars + 2 padding
		wStatus = 8  // "Status" (6) padded to 8; ✓/✗ centred within
		wMs     = 3  // duration digits min width
	)
	wWhen, wTool, wName := calcRecentWidths(recent)

	headerWidth := wWhen + wTool + wMs + 2 + wStatus + 2 + wSessID + 2 + wName
	fmt.Println(tui.SepStyle.Render(strings.Repeat("╌", headerWidth)))
	fmt.Printf("%s%s%s  %s  %s  %s\n",
		padRight(tui.HintStyle.Render("When"), wWhen),
		padRight(tui.HintStyle.Render("Tool"), wTool),
		padRight(tui.HintStyle.Render("ms"), wMs),
		padRight(tui.HintStyle.Render("Status"), wStatus),
		padRight(tui.HintStyle.Render("Session"), wSessID),
		tui.HintStyle.Render("Name"),
	)
	fmt.Println(tui.SepStyle.Render(strings.Repeat("╌", headerWidth)))

	termWidth := 80
	if w, _, err := term.GetSize(uintptr(os.Stdout.Fd())); err == nil && w > 0 {
		termWidth = w
	}
	errMaxWidth := max(termWidth-wWhen-2, 40)

	for _, c := range recent {
		renderRecentCallRow(c, wWhen, wTool, wMs, wStatus, wSessID, wName, errMaxWidth)
	}

	return nil
}

func statsToolSummaryTable(db *stats.DB, filter stats.Filter) (string, error) {
	summary, err := db.Summary(filter)
	if err != nil {
		return "", fmt.Errorf("querying summary: %w", err)
	}

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
	return t1.Render(), nil
}

func calcRecentWidths(recent []stats.RecentCall) (wWhen, wTool, wName int) {
	wWhen = 8 // "When"
	wTool = 4 // "Tool"
	wName = 7 // "Name" (session human name)
	for _, c := range recent {
		if l := len(humanAge(c.CalledAt)); l > wWhen {
			wWhen = l
		}
		if l := len(c.Tool); l > wTool {
			wTool = l
		}
		if l := len(c.SessionName); l > wName {
			wName = l
		}
	}
	return wWhen + 2, wTool + 2, wName
}

func renderRecentCallRow(c stats.RecentCall, wWhen, wTool, wMs, wStatus, wSessID, wName, errMaxWidth int) {
	ok := tui.OkStyle.Render("✓")
	if !c.Success {
		ok = tui.WarnStyle.Render("✗")
	}
	sessID := padRight(shortSessionID(c.SessionID), wSessID)
	name := tui.MutedStyle.Render(c.SessionName)
	when := padRight(humanAge(c.CalledAt), wWhen)
	tool := padRight(c.Tool, wTool)
	ms := padRight(fmt.Sprintf("%d", c.DurationMs), wMs)
	status := centerStr(ok, wStatus)

	if !c.Success {
		fmt.Println(tui.WarnStyle.Render(when+tool+ms) + "  " + status + "  " + tui.MutedStyle.Render(sessID) + "  " + name)
	} else {
		fmt.Println(when + tool + ms + "  " + status + "  " + tui.MutedStyle.Render(sessID) + "  " + name)
	}

	if !c.Success && c.ErrorMsg != "" {
		lines := strings.Split(c.ErrorMsg, "\n")
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			wrapped := wordwrap.String(line, errMaxWidth)
			wrappedLines := strings.Split(wrapped, "\n")
			for j, wl := range wrappedLines {
				prefix := "  "
				if i == 0 && j == 0 {
					prefix = "↳ "
				}
				fmt.Printf("%*s%s\n", wWhen, "", tui.WarnStyle.Render(prefix+wl))
			}
		}
	}
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
