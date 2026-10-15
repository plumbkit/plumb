package cli

// stats_context.go — `plumb stats --context`: the context-pack report
// (stats.ContextReport), week by week, for a short-to-medium horizon of a week
// to a couple of months of use. Read-only; everything is derived from the calls
// plumb already records.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/stats"
)

// statsContextDefaultWindow is the window when --since is not given: long
// enough to show a trend, short enough that last quarter's habits do not drown
// this month's.
const statsContextDefaultWindow = 8 * 7 * 24 * time.Hour

// statsContextMaxWeeks is how many week columns the table shows; the total
// column always covers the whole window.
const statsContextMaxWeeks = 8

var statsFlagContext, statsFlagJSON, statsFlagAllWorkspaces bool

func init() {
	statsCmd.Flags().BoolVar(&statsFlagContext, "context", false,
		"show the context_for_task report (uptake, pack recall and use, edit and test outcomes with and without a pack, and leads) week by week; --since defaults to 8w")
	statsCmd.Flags().BoolVar(&statsFlagJSON, "json", false, "with --context, print the report as JSON")
	statsCmd.Flags().BoolVar(&statsFlagAllWorkspaces, "all-workspaces", false,
		"with --context, count every workspace's calls, worktrees and scratch checkouts included, not only --workspace's")
}

// checkContextFlagCompat refuses --context with a flag it has no use for, and
// --json without --context.
func checkContextFlagCompat(cmd *cobra.Command) error {
	if !statsFlagContext {
		if statsFlagJSON || statsFlagAllWorkspaces {
			return errors.New("--json and --all-workspaces are only used with --context")
		}
		return nil
	}
	var set []string
	for _, name := range []string{"health", "failures", "limit"} {
		if cmd.Flags().Changed(name) {
			set = append(set, "--"+name)
		}
	}
	if len(set) > 0 {
		return fmt.Errorf("--context has its own view: %s cannot be combined with it", strings.Join(set, ", "))
	}
	return nil
}

// runStatsContext builds the report for ws over --since (default 8 weeks).
func runStatsContext(ws string, out io.Writer) error {
	window := statsContextDefaultWindow
	if statsFlagSince != "" {
		w, err := parseAge(statsFlagSince)
		if err != nil {
			return err
		}
		window = w
	}
	db, err := stats.OpenReadOnly()
	if err != nil {
		return fmt.Errorf("opening stats db: %w", err)
	}
	if db == nil {
		fmt.Fprintln(out, "No statistics recorded yet.")
		return nil
	}
	defer db.Close()
	now := time.Now()
	filter := stats.Filter{Workspace: ws, Since: now.Add(-window)}
	if statsFlagAllWorkspaces {
		filter.Workspace, ws = "", "all workspaces"
	}
	report, err := db.ContextReport(filter, now)
	if err != nil {
		return err
	}
	if statsFlagJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	renderContextReport(out, ws, report)
	return nil
}

// ratio renders n of d as "n/d (p%)", or "—" when there is nothing to divide.
func ratio(n, d int) string {
	if d == 0 {
		return "—"
	}
	return fmt.Sprintf("%d/%d (%d%%)", n, d, (100*n+d/2)/d)
}

// perStream renders a count per stream to one decimal.
func perStream(n, d int) string {
	if d == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f", float64(n)/float64(d))
}

// contextRow is one metric across the shown weeks and the total.
type contextReportRow struct {
	label string
	cell  func(p stats.ContextPeriod) string
}

var contextReportRows = []contextReportRow{
	{"Uptake", nil},
	{"  agent streams", func(p stats.ContextPeriod) string { return strconv.Itoa(p.Streams) }},
	{"  streams with a pack", func(p stats.ContextPeriod) string { return ratio(p.StreamsWithPack, p.Streams) }},
	{"  packs (from task prose)", func(p stats.ContextPeriod) string { return fmt.Sprintf("%d (%d)", p.Packs, p.ProsePacks) }},
	{"Pack quality", nil},
	{"  edited files the pack named", func(p stats.ContextPeriod) string { return ratio(p.EditedFilesInPack, p.EditedFiles) }},
	{"  pack files read or edited", func(p stats.ContextPeriod) string { return ratio(p.PackFilesUsed, p.PackFiles) }},
	{"  proposed seeds used", func(p stats.ContextPeriod) string { return ratio(p.ProposedSeedsUsed, p.ProposedSeeds) }},
	{"  searches before 1st edit, per pack", func(p stats.ContextPeriod) string { return perStream(p.SearchesBeforeEdit, p.WindowsWithEdits) }},
	{"  seed files re-read", func(p stats.ContextPeriod) string { return strconv.Itoa(p.SeedRereads) }},
	{"Edits with a pack | without", nil},
	{"  editing streams", func(p stats.ContextPeriod) string { return pair(p.WithPack.Streams, p.WithoutPack.Streams) }},
	{"  failed edits", func(p stats.ContextPeriod) string {
		return pairRatio(p.WithPack.EditFailures, p.WithPack.Edits, p.WithoutPack.EditFailures, p.WithoutPack.Edits)
	}},
	{"  edits adding compile errors", func(p stats.ContextPeriod) string {
		return pairRatio(p.WithPack.NewErrorEdits, p.WithPack.DiagnosedEdits, p.WithoutPack.NewErrorEdits, p.WithoutPack.DiagnosedEdits)
	}},
	{"  undos per editing stream", func(p stats.ContextPeriod) string {
		return perStream(p.WithPack.Undos, p.WithPack.Streams) + " | " + perStream(p.WithoutPack.Undos, p.WithoutPack.Streams)
	}},
	{"  edits per file edited", func(p stats.ContextPeriod) string {
		return perStream(p.WithPack.Edits, p.WithPack.FilesEdited) + " | " + perStream(p.WithoutPack.Edits, p.WithoutPack.FilesEdited)
	}},
	{"  failed test runs", func(p stats.ContextPeriod) string {
		return pairRatio(p.WithPack.TestFailures, p.WithPack.TestRuns, p.WithoutPack.TestFailures, p.WithoutPack.TestRuns)
	}},
	{"  tested streams ending green", func(p stats.ContextPeriod) string {
		return pairRatio(p.WithPack.EndedGreen, p.WithPack.StreamsWithTests, p.WithoutPack.EndedGreen, p.WithoutPack.StreamsWithTests)
	}},
}

func pair(a, b int) string { return fmt.Sprintf("%d | %d", a, b) }

func pairRatio(n1, d1, n2, d2 int) string { return ratio(n1, d1) + " | " + ratio(n2, d2) }

// renderContextReport prints the weeks (the latest statsContextMaxWeeks) and the
// total as columns, then the leads and how to read the numbers.
func renderContextReport(out io.Writer, ws string, r stats.ContextReport) {
	fmt.Fprintf(out, "context_for_task report — %s, %s to %s (UTC weeks from Monday)\n\n",
		render.ContractPath(ws), r.Since.UTC().Format("2006-01-02"), r.Until.UTC().Format("2006-01-02"))
	if r.Total.Streams == 0 {
		fmt.Fprintln(out, "No plumb calls recorded for this workspace in the window.")
		return
	}
	weeks := r.Weeks
	if len(weeks) > statsContextMaxWeeks {
		weeks = weeks[len(weeks)-statsContextMaxWeeks:]
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := []string{"metric"}
	for _, w := range weeks {
		header = append(header, "wk "+w.Start.Format("01-02"))
	}
	header = append(header, "total")
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, row := range contextReportRows {
		cells := []string{row.label}
		if row.cell != nil {
			for _, w := range weeks {
				cells = append(cells, row.cell(w))
			}
			cells = append(cells, row.cell(r.Total))
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
	renderContextLeads(out, ws, r.Leads)
	fmt.Fprint(out, contextReportFootnote)
}

func renderContextLeads(out io.Writer, ws string, l stats.ContextLeads) {
	section := func(title string, cs []stats.ContextCount) {
		if len(cs) == 0 {
			return
		}
		fmt.Fprintf(out, "\n%s\n", title)
		for _, c := range cs {
			fmt.Fprintf(out, "  %4d  %s\n", c.Count, strings.TrimPrefix(c.Text, ws+"/"))
		}
	}
	fmt.Fprintln(out, "\nLeads — what agents needed that packs did not give")
	section("Files edited after a pack that the pack did not name:", l.MissedFiles)
	section("Searches run after a pack, before the first edit:", l.Searches)
	section("Gaps packs admitted most often:", l.Gaps)
	if len(l.Feedback) > 0 {
		fmt.Fprintln(out, "\nNotes agents left with tool_feedback (newest first):")
		for _, f := range l.Feedback {
			var parts []string
			for _, kv := range [][2]string{{"missing", f.Missing}, {"noise", f.Noise}, {"note", f.Note}} {
				if kv[1] != "" {
					parts = append(parts, kv[0]+": "+kv[1])
				}
			}
			fmt.Fprintf(out, "  %s  %s: %s\n", f.At.UTC().Format("2006-01-02"), f.Tool, strings.Join(parts, "; "))
		}
	}
}

const contextReportFootnote = `
How to read it: an agent stream is one session, or one logical agent on a shared
connection. A pack's window runs from the pack to the stream's next pack or its
end. Only plumb calls are recorded: native reads, searches and edits are not, so
every rate is over plumb's lane. "With a pack" and "without" compare different
streams doing different work; read a gap between them as a lead, not a verdict.
Compile errors come from the post-write diagnostics an edit asked for.
`

// statsContextOut is where the report is written; a var so tests capture it.
var statsContextOut io.Writer = os.Stdout
