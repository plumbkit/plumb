package cli

// stats_failures.go — the `plumb stats --failures` triage view.

import (
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/tui"
)

// printStatsFailures renders failures grouped by kind, tool and client build.
// It replaces the default per-tool table rather than joining it: see the flag's
// registration for why the two views do not share a grain.
func printStatsFailures(w io.Writer, db *stats.DB, filter stats.Filter, limit int, ws string) error {
	buckets, err := db.FailureSummary(limit, filter)
	if err != nil {
		return fmt.Errorf("querying failure summary: %w", err)
	}
	if len(buckets) == 0 {
		printCLIDiagnostic(w, cliDiagnostic{
			Kind:  "info",
			Title: "No failures recorded",
			Body:  fmt.Sprintf("Every recorded call for %s succeeded.", render.ContractPath(ws)) + sinceSuffix(filter),
		})
		return nil
	}

	fmt.Fprintln(w, "Failures by Kind"+sinceSuffix(filter))
	fmt.Fprintln(w, statsFailureTable(buckets))
	if note := unclassifiedNote(buckets); note != "" {
		fmt.Fprintln(w, tui.MutedStyle.Render(note))
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

// sinceSuffix names the window a view was scoped to, so a reader is never left
// guessing whether a count covers an hour or the whole history of the database.
func sinceSuffix(filter stats.Filter) string {
	if filter.Since.IsZero() {
		return ""
	}
	return " (last " + humanWindow(time.Since(filter.Since)) + ")"
}

// humanWindow labels a --since window in the largest unit that divides it
// exactly, so "7d" comes back as "7d" rather than "168h0m0.0002s" — and "90m"
// stays "90m" rather than rounding away to "1h".
//
// The duration is measured as time.Since(filter.Since), which overshoots the
// requested window by however long the query took, so it is rounded to the
// second before the divisibility test; without that no unit ever divides evenly.
func humanWindow(d time.Duration) string {
	d = d.Round(time.Second)
	for _, u := range []struct {
		size   time.Duration
		suffix string
	}{
		{24 * time.Hour, "d"},
		{time.Hour, "h"},
		{time.Minute, "m"},
	} {
		if d >= u.size && d%u.size == 0 {
			return strconv.FormatInt(int64(d/u.size), 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(d/time.Second), 10) + "s"
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
func unclassifiedNote(buckets []stats.FailureCount) string {
	var n int64
	for _, f := range buckets {
		if f.Kind == "" {
			n += f.Calls
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("↳ %d %s no classification: recorded before the failure columns "+
		"existed, or a failure plumb makes no structured claim about. Nothing is "+
		"inferred from the error text.", n, textfmt.Plural(n, "failure carries", "failures carry"))
}
