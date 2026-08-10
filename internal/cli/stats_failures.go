package cli

// stats_failures.go — the `plumb stats --failures` triage view.

import (
	"fmt"
	"io"
	"strconv"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/tui"
)

// statsView carries the presentation choices runStats resolved from its flags.
//
// since is the RAW `--since` text the user typed, deliberately not a duration or
// a timestamp. The window label has to echo the request; re-deriving it from
// filter.Since at render time measures wall-clock instead, which grows by
// however long the queries took — and on the never-pruned database `--since`
// exists to tame, three full-table aggregates can easily exceed a second, at
// which point "7d" renders as "604801s". Carrying the text cannot drift, and it
// also keeps "2w" reading as 2w rather than 14d.
type statsView struct {
	workspace string
	limit     int
	since     string
}

// sinceSuffix names the window a view was scoped to, so a reader is never left
// guessing whether a count covers an hour or the whole history of the database.
func (v statsView) sinceSuffix() string {
	if v.since == "" {
		return ""
	}
	return " (last " + v.since + ")"
}

// printStatsFailures renders failures grouped by kind, tool and client build.
// It replaces the default per-tool table rather than joining it: see the flag's
// registration for why the two views do not share a grain.
func printStatsFailures(w io.Writer, db *stats.DB, filter stats.Filter, v statsView) error {
	report, err := db.FailureSummary(v.limit, filter)
	if err != nil {
		return fmt.Errorf("querying failure summary: %w", err)
	}
	if len(report.Buckets) == 0 {
		printCLIDiagnostic(w, cliDiagnostic{
			Kind:  "info",
			Title: "No failures recorded",
			Body: fmt.Sprintf("Every recorded call for %s succeeded%s.",
				render.ContractPath(v.workspace), v.sinceSuffix()),
		})
		return nil
	}

	fmt.Fprintln(w, "Failures by Kind"+v.sinceSuffix())
	fmt.Fprintln(w, statsFailureTable(report.Buckets))
	for _, note := range []string{unclassifiedNote(report), truncationNote(report)} {
		if note != "" {
			fmt.Fprintln(w, tui.MutedStyle.Render(note))
		}
	}
	return nil
}

func statsFailureTable(buckets []stats.FailureCount) string {
	t := render.DottedTableBase(tui.SepStyle, tui.HintStyle).
		Headers("Kind", "Tool", "Client", "Calls", "Retryable")
	for _, f := range buckets {
		t.Row(
			f.Label(),
			f.Tool,
			statsClientCell(f),
			strconv.FormatInt(f.Calls, 10),
			retryableCell(f),
		)
	}
	return t.Render()
}

// retryableCell renders how many of a bucket's calls were retryable — or an em
// dash for the unclassified bucket, where the honest answer is "unknown". A
// literal 0 there would read as "we checked, none of them were", which is a
// claim about rows plumb never classified in the first place.
func retryableCell(f stats.FailureCount) string {
	if f.Kind == "" {
		return "—"
	}
	return strconv.FormatInt(f.Retryable, 10)
}

// statsClientCell renders the client build that made the failing calls, or an
// em dash when the client never identified itself.
func statsClientCell(f stats.FailureCount) string {
	switch {
	case f.ClientName == "":
		return "—"
	case f.ClientVersion == "":
		return f.ClientName
	}
	return f.ClientName + " " + f.ClientVersion
}

// unclassifiedNote explains the unclassified bucket when one is present, so a
// reader does not mistake it for a kind plumb chose. Silence otherwise.
//
// The count comes from the whole-filter total, never from the rendered buckets:
// those are capped, so counting them would under-report the very thing the note
// exists to be honest about.
func unclassifiedNote(r stats.FailureReport) string {
	n := r.UnclassifiedCalls
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("↳ %d %s no classification: recorded before the failure columns "+
		"existed, or a failure plumb makes no structured claim about. Nothing is "+
		"inferred from the error text.", n, textfmt.Plural(n, "failure carries", "failures carry"))
}

// truncationNote says that a bounded view is bounded. Without it a reader takes
// the rows on screen for the whole picture — three buckets of five hundred and
// five calls looks exactly like three buckets of three.
func truncationNote(r stats.FailureReport) string {
	if !r.Truncated() {
		return ""
	}
	return fmt.Sprintf("↳ showing %d of %d %s (%d of %d failed calls); raise --limit, or narrow with --since.",
		len(r.Buckets), r.TotalBuckets, textfmt.Plural(r.TotalBuckets, "bucket", "buckets"),
		r.ShownCalls(), r.TotalCalls)
}
