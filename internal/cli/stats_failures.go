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

// printStatsFailures renders failures grouped by kind, tool and client build.
// It replaces the default per-tool table rather than joining it: see the flag's
// registration for why the two views do not share a grain.
func printStatsFailures(w io.Writer, db *stats.DB, filter stats.Filter, ws string) error {
	buckets, err := db.FailureSummary(filter)
	if err != nil {
		return fmt.Errorf("querying failure summary: %w", err)
	}
	if len(buckets) == 0 {
		printCLIDiagnostic(w, cliDiagnostic{
			Kind:  "info",
			Title: "No failures recorded",
			Body:  fmt.Sprintf("Every recorded call for %s succeeded.", render.ContractPath(ws)),
		})
		return nil
	}

	fmt.Fprintln(w, "Failures by Kind")
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
			strconv.FormatInt(f.Retryable, 10),
		)
	}
	return t.Render()
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
