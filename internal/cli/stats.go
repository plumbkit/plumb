package cli

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/reflow/wordwrap"
	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/tui"
)

var (
	statsFlagWorkspace string
	statsFlagLimit     int
	statsFlagFailures  bool
	statsFlagSince     string
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
	// A flag rather than an extra section, because the failure breakdown is a
	// different GRAIN, not more columns: the default table is one row per tool,
	// while a failure bucket is (kind × tool × client build). Folding it in
	// would either duplicate tool rows or drop the kind — the one fact the view
	// exists for — and the default table already carries nine columns. Triage is
	// also a distinct occasion: you reach for it when something is failing, not
	// on every `plumb stats`.
	statsCmd.Flags().BoolVar(&statsFlagFailures, "failures", false, "show failures grouped by kind, tool and client instead of the default view")
	// `tool_calls` is never pruned, so without a window every view eventually
	// reports mostly history. It matters most to --failures, where years of
	// pre-classification rows would otherwise dominate, but the same is true of
	// the per-tool averages, so the window scopes the whole command.
	statsCmd.Flags().StringVar(&statsFlagSince, "since", "", "only count calls newer than this age, e.g. 24h, 7d, 2w (default: all history)")
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

	// Filter stats to the requested workspace, and to the requested window.
	view := statsView{workspace: ws, limit: statsFlagLimit, since: statsFlagSince}
	filter := stats.Filter{Workspace: ws}
	if statsFlagSince != "" {
		window, err := parseAge(statsFlagSince)
		if err != nil {
			return err
		}
		filter.Since = time.Now().Add(-window)
	}

	total := db.TotalCalls(filter)
	if total == 0 {
		printCLIDiagnostic(os.Stdout, cliDiagnostic{
			Kind:  "info",
			Title: "No statistics recorded yet",
			Body:  fmt.Sprintf("No statistics for workspace %s%s.", render.ContractPath(ws), view.sinceSuffix()),
		})
		return nil
	}

	tui.RebuildStyles()

	// PLAN-367: scoped to the CURRENT savings-model version only. Rows scored
	// under an earlier version measured a different counterfactual (v4 stopped
	// crediting a capable client's plain ranged read — see
	// clientcaps.ModelVersion) and must never be summed alongside it as one
	// figure. The tool-schema surcharge line session_start shows is deliberately
	// absent here: it is computed from a LIVE MCP connection's advertised tool
	// set, which this offline stats reader has no access to.
	versionedFilter := filter
	versionedFilter.SavingsModelVersion = clientcaps.ModelVersion
	axes := db.SavingsAxes(versionedFilter)
	prevented := db.PreventedIncidents(filter)

	summaryLine := fmt.Sprintf("↳ %d total calls · ~%s tokens estimated read savings (model v%d, since this version only) · %d prevented incidents",
		total, stats.FormatSavings(int(axes.Total())), clientcaps.ModelVersion, prevented)

	// Structured Context Block
	fmt.Println(render.ContextBox(
		fmt.Sprintf("%s\n%s",
			render.ContractPath(ws),
			tui.MutedStyle.Render(summaryLine),
		),
		tui.SepStyle,
	))
	fmt.Println()

	if statsFlagFailures {
		return printStatsFailures(os.Stdout, db, filter, view)
	}

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
		render.PadRight(tui.HintStyle.Render("When"), wWhen),
		render.PadRight(tui.HintStyle.Render("Tool"), wTool),
		render.PadRight(tui.HintStyle.Render("ms"), wMs),
		render.PadRight(tui.HintStyle.Render("Status"), wStatus),
		render.PadRight(tui.HintStyle.Render("Session"), wSessID),
		tui.HintStyle.Render("Name"),
	)
	fmt.Println(tui.SepStyle.Render(strings.Repeat("╌", headerWidth)))

	termWidth := 80
	if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
		termWidth = w
	}
	errMaxWidth := max(termWidth-wWhen-2, 40)

	for _, c := range recent {
		renderRecentCallRow(c, wWhen, wTool, wMs, wStatus, wSessID, errMaxWidth)
	}

	return nil
}

// parseAge reads a --since value as an age. It accepts Go's own duration syntax
// plus the day and week suffixes a reader reaches for when scoping months of
// stats history, because "168h" is not what anyone means by a week.
func parseAge(s string) (time.Duration, error) {
	if unit, ok := strings.CutSuffix(s, "d"); ok {
		return scaledAge(s, unit, "days", 24*time.Hour)
	}
	if unit, ok := strings.CutSuffix(s, "w"); ok {
		return scaledAge(s, unit, "weeks", 7*24*time.Hour)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--since %q: expected an age like 90m, 24h, 7d or 2w", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("--since %q: expected a positive age", s)
	}
	return d, nil
}

// scaledAge multiplies a whole number of units into a Duration, refusing a
// product that would not fit one.
//
// The overflow check is on the PRODUCT, not just the input: a Duration is an
// int64 of nanoseconds, so "9223372036854775807d" wraps to a NEGATIVE duration,
// which would set the window's start in the FUTURE and report an empty database
// on a full one — a wrong answer delivered confidently, which is worse than the
// error the input deserves.
func scaledAge(orig, n, unitName string, unit time.Duration) (time.Duration, error) {
	v, err := strconv.Atoi(n)
	if err != nil || v <= 0 || int64(v) > int64(math.MaxInt64)/int64(unit) {
		return 0, fmt.Errorf("--since %q: expected a positive whole number of %s, small enough to be a real age", orig, unitName)
	}
	return time.Duration(v) * unit, nil
}

func statsToolSummaryTable(db *stats.DB, filter stats.Filter) (string, error) {
	summary, err := db.Summary(filter)
	if err != nil {
		return "", fmt.Errorf("querying summary: %w", err)
	}

	t1 := render.DottedTableBase(tui.SepStyle, tui.HintStyle).
		Headers("Tool", "Calls", "Avg ms", "P95 ms", "Input", "Output", "Errors", "Capability", "Efficiency")

	for _, s := range summary {
		t1.Row(
			s.Tool,
			strconv.FormatInt(s.Calls, 10),
			fmt.Sprintf("%.0f", s.AvgMs),
			strconv.FormatInt(s.P95Ms, 10),
			fmt.Sprintf("%.1f KB", s.TotalInputKB),
			fmt.Sprintf("%.1f KB", s.TotalOutputKB),
			strconv.FormatInt(s.Errors, 10),
			axisCell(s.CapabilityTokens),
			axisCell(s.EfficiencyTokens),
		)
	}
	return t1.Render(), nil
}

// axisCell renders one savings-axis token count for the stats table: an em dash
// when zero, else a short approximate count.
func axisCell(tokens int64) string {
	if tokens <= 0 {
		return "—"
	}
	return "~" + stats.FormatSavings(int(tokens))
}

func calcRecentWidths(recent []stats.RecentCall) (wWhen, wTool, wName int) {
	wWhen = 8 // "When"
	wTool = 4 // "Tool"
	wName = 7 // "Name" (session human name)
	for _, c := range recent {
		if l := len(render.HumanAge(c.CalledAt)); l > wWhen {
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

func renderRecentCallRow(c stats.RecentCall, wWhen, wTool, wMs, wStatus, wSessID, errMaxWidth int) {
	ok := tui.OkStyle.Render("✓")
	if !c.Success {
		ok = tui.WarnStyle.Render("✗")
	}
	sessID := render.PadRight(shortSessionID(c.SessionID), wSessID)
	name := tui.MutedStyle.Render(c.SessionName)
	when := render.PadRight(render.HumanAge(c.CalledAt), wWhen)
	tool := render.PadRight(c.Tool, wTool)
	ms := render.PadRight(strconv.FormatInt(c.DurationMs, 10), wMs)
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
