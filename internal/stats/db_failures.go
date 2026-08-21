package stats

// db_failures.go — the failure breakdown: how plumb's refusals and faults
// distribute across kinds, tools and client builds.

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// UnclassifiedLabel is how a blank error_kind renders. A blank kind means one
// of exactly two things, and both deserve this label rather than a guess: the
// row predates schema v14, or the failure carried no classification and plumb
// therefore told the client nothing structured either (no `_meta` envelope).
//
// It is deliberately NOT "internal". toolerror.KindInternal is a real,
// deliberate classification — "this is a fault, and we know that much" — and
// folding the unclassified into it would inflate the one bucket a reader uses
// to decide whether plumb itself is broken.
const UnclassifiedLabel = "unclassified"

// defaultFailureBuckets bounds a FailureSummary that names no limit. It is a
// backstop against the caller-supplied client identity in the grouping keys
// (see FailureCount), not a display preference — a caller that wants fewer rows
// should say so.
const defaultFailureBuckets = 100

// unclassifiedBucketCap bounds the unclassified side of the split. It is small
// because the breakdown there is barely actionable — "40 failures in git that
// plumb could not classify" is not a finding you can act on the way a kind is —
// and because the honest total always comes from
// FailureReport.UnclassifiedCalls rather than from these rows.
const unclassifiedBucketCap = 10

// FailureCount is one bucket of the failure breakdown.
//
// Two of the four grouping keys are low-cardinality by construction: Kind is a
// closed thirteen-value set and Tool is the fixed registry. The other two are
// NOT. ClientName and ClientVersion are copied verbatim out of the MCP
// `initialize` frame's `clientInfo`, with no validation, normalisation or length
// cap anywhere on that path, so a client that varies its version string varies
// the bucket count with it — 500 distinct versions produce 500 buckets. That is
// why FailureSummary takes a limit and applies it in SQL rather than trusting
// the shape of the data; do not remove it on the strength of "these are only
// client builds".
//
// Retryable counts how many of Calls were recorded as retryable. The
// remediation class is not a grouping key — a single kind can be reached from
// more than one seam and so from more than one class — and a per-row class
// would either need its own bucket or be picked arbitrarily from the group;
// the retryable count is the honest summary of the same fact.
type FailureCount struct {
	Kind          toolerror.Kind
	Tool          string
	ClientName    string
	ClientVersion string
	Calls         int64
	Retryable     int64
}

// Label names this bucket for display: the kind, or UnclassifiedLabel when the
// row carries no structured claim.
func (f FailureCount) Label() string {
	if f.Kind == "" {
		return UnclassifiedLabel
	}
	return string(f.Kind)
}

// FailureReport is a BOUNDED view of the failures matching a filter, together
// with the whole-filter totals it was bounded against.
//
// The totals are not derived from Buckets and must not be: Buckets is truncated
// by the limit, so counting it would silently under-report by exactly the amount
// the limit removed. Every count a reader is shown — the unclassified note, the
// truncation footer — comes from these fields, which are computed over the whole
// filter regardless of what the view shows.
type FailureReport struct {
	Buckets []FailureCount

	// TotalBuckets is how many buckets match the filter, before the limit —
	// both classified and unclassified.
	TotalBuckets int64
	// ClassifiedBuckets is how many of TotalBuckets are classified — the side
	// --limit actually governs. Kept separate from TotalBuckets so a caller can
	// tell WHICH side of the split a truncated view was cut on: the classified
	// side responds to --limit, the unclassified side is capped independently
	// (unclassifiedBucketCap) and --limit cannot widen it.
	ClassifiedBuckets int64
	// TotalCalls is how many failed calls match the filter, in all buckets.
	TotalCalls int64
	// UnclassifiedCalls is how many of TotalCalls carry no classification —
	// counted over the whole filter, so it stays right even when the
	// unclassified buckets themselves are truncated.
	UnclassifiedCalls int64
}

// Incomplete reports whether Buckets omits a bucket the filter matched. A view
// that is bounded must say so; a footer built on this is the only thing standing
// between a reader and quietly reading 3 buckets of 505 as the whole picture.
//
// Named for the view rather than the cut ("Truncated") because the shared-
// primitives rule reserves truncate-prefixed names for internal/textfmt's string
// helpers, and this is a count comparison, not one of those.
func (r FailureReport) Incomplete() bool { return int64(len(r.Buckets)) < r.TotalBuckets }

// ClassifiedTruncated reports whether the LIMIT-governed classified side of the
// split is what got cut, as opposed to the unclassified side, which is capped
// independently (unclassifiedBucketCap) and unaffected by --limit. A footer
// that recommends raising --limit is only correct when this is true.
func (r FailureReport) ClassifiedTruncated() bool {
	var shown int64
	for _, f := range r.Buckets {
		if f.Kind != "" {
			shown++
		}
	}
	return shown < r.ClassifiedBuckets
}

// ShownCalls sums the calls covered by Buckets, for a footer that can state what
// fraction of the failures the view actually accounts for.
func (r FailureReport) ShownCalls() int64 {
	var n int64
	for _, f := range r.Buckets {
		n += f.Calls
	}
	return n
}

// FailureSummary returns the n busiest CLASSIFIED failure buckets matching
// filter, grouped by kind, tool and client build, plus the unclassified buckets
// and the whole-filter totals. n <= 0 applies defaultFailureBuckets.
//
// CLASSIFIED BUCKETS COME FIRST, ahead of the unclassified ones regardless of
// size. `tool_calls` is never pruned, so on any installation with history the
// pre-v14 failures outnumber the classified ones for a long time — ordering
// purely by count buries every actionable bucket under a row that says only
// "this predates the feature".
//
// THE UNCLASSIFIED BUCKETS ARE FETCHED SEPARATELY, so the limit cannot delete
// them. Ordering classified-first and then applying one LIMIT would have made
// the unclassified bucket the FIRST thing cut — the fix quietly undoing the
// promise below. They carry their own small cap instead, because they are
// grouped by tool and client too and are no more bounded than the classified
// ones; when that cap bites, the counts a reader sees still come from
// UnclassifiedCalls, which is computed over every matching row.
//
// So the unclassified failures are always reported, never dropped: a report that
// silently omitted them would understate the failure count by exactly the amount
// it understands least.
func (d *DB) FailureSummary(n int, filter Filter) (FailureReport, error) {
	if d == nil {
		return FailureReport{}, nil
	}
	if n <= 0 {
		n = defaultFailureBuckets
	}
	where, args := filter.where()
	if where == "" {
		where = " WHERE success = 0"
	} else {
		where += " AND success = 0"
	}

	report, err := d.failureTotals(where, args)
	if err != nil {
		return FailureReport{}, err
	}
	classified, err := d.failureBuckets(where+" AND error_kind <> ''", args, n)
	if err != nil {
		return FailureReport{}, err
	}
	unclassified, err := d.failureBuckets(where+" AND error_kind = ''", args, unclassifiedBucketCap)
	if err != nil {
		return FailureReport{}, err
	}
	report.Buckets = append(classified, unclassified...)
	return report, nil
}

// failureTotals counts the whole filter in one pass: buckets, failed calls, and
// the unclassified share. The outer aggregate over the grouped subquery is what
// makes the bucket count exact without concatenating the grouping keys into a
// synthetic one.
func (d *DB) failureTotals(where string, args []any) (FailureReport, error) {
	//nolint:gosec // G202: where is built by filter.where() using ? placeholders only; no user values interpolated
	q := `SELECT COUNT(*), COALESCE(SUM(calls), 0), COALESCE(SUM(unclassified), 0),
	             COALESCE(SUM(CASE WHEN kind <> '' THEN 1 ELSE 0 END), 0) FROM (
	          SELECT error_kind AS kind, COUNT(*) AS calls,
	                 SUM(CASE WHEN error_kind = '' THEN 1 ELSE 0 END) AS unclassified
	          FROM tool_calls` + where + `
	          GROUP BY error_kind, tool, client_name, client_version)`
	var r FailureReport
	if err := d.db.QueryRow(q, args...).Scan(&r.TotalBuckets, &r.TotalCalls, &r.UnclassifiedCalls, &r.ClassifiedBuckets); err != nil {
		return FailureReport{}, fmt.Errorf("stats: failure totals: %w", err)
	}
	return r, nil
}

// preventedIncidentKinds are the failure classifications that represent
// plumb catching something that would otherwise have silently gone wrong: a
// caller writing over a version it never read or that changed since
// (KindUnreadOrStale — covers both the read-before-write "strict mode" save
// and the optimistic-concurrency "modified since read" rejection, which share
// one guard), a write refused over pre-existing uncommitted changes
// (KindDirtyFile — the closest existing signal to "someone else's work is
// here"), and the cross-session ref-movement guard (KindConcurrentRefMove —
// a peer moved HEAD/branch since this session last observed it). This is a
// deliberately conservative list: PLAN-367 also names "exactly-once ambiguity
// rejections" (edit_file's old_string-must-be-unique refusal) as a prevented
// incident, but that refusal is not yet classified under its own Kind — it
// currently falls into the broad, unrelated-things-included KindInternal
// bucket via editLogicErr, and counting that bucket here would inflate N with
// ordinary internal faults it was never meant to include. Left as a follow-up
// (recorded in the PLAN-367 card Log) rather than added by widening a bucket
// past what it can support.
var preventedIncidentKinds = []toolerror.Kind{
	toolerror.KindUnreadOrStale,
	toolerror.KindDirtyFile,
	toolerror.KindConcurrentRefMove,
}

// PreventedIncidents counts failed CALLS (not distinct incidents — a caller
// that retries after a refusal contributes one count per refusal) matching
// filter whose error_kind is one plumb's write guards use to stop a caller
// from silently clobbering something — see preventedIncidentKinds. This is
// the number the PLAN-367 banner reports as "guard refusals": unlike the
// savings axes, it is not an estimate reconstructed from a counterfactual
// model — it is a direct count of refusals the daemon actually issued, so it
// carries no model version and needs no version filter.
func (d *DB) PreventedIncidents(filter Filter) int64 {
	if d == nil {
		return 0
	}
	where, args := filter.where()
	placeholders := make([]string, len(preventedIncidentKinds))
	for i, k := range preventedIncidentKinds {
		placeholders[i] = "?"
		args = append(args, string(k))
	}
	clause := "success = 0 AND error_kind IN (" + strings.Join(placeholders, ",") + ")"
	if where == "" {
		where = " WHERE " + clause
	} else {
		where += " AND " + clause
	}
	//nolint:gosec // G202: where is built from filter.where() plus a fixed IN(...) of ? placeholders only
	q := `SELECT COUNT(*) FROM tool_calls` + where
	var n int64
	if err := d.db.QueryRow(q, args...).Scan(&n); err != nil {
		slog.Warn("stats: prevented incidents count failed", "err", err)
		return 0
	}
	return n
}

// failureBuckets runs the grouped query for one side of the classified split.
func (d *DB) failureBuckets(where string, args []any, limit int) ([]FailureCount, error) {
	//nolint:gosec // G202: where is built by filter.where() using ? placeholders only; no user values interpolated
	q := `SELECT error_kind, tool, client_name, client_version,
	             COUNT(*) AS calls,
	             COALESCE(SUM(error_retryable), 0) AS retryable
	      FROM tool_calls` + where + `
	      GROUP BY error_kind, tool, client_name, client_version
	      ORDER BY calls DESC, error_kind, tool
	      LIMIT ?`
	rows, err := d.db.Query(q, append(append([]any{}, args...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("stats: failure summary: %w", err)
	}
	defer rows.Close()

	var out []FailureCount
	for rows.Next() {
		var f FailureCount
		if err := rows.Scan(&f.Kind, &f.Tool, &f.ClientName, &f.ClientVersion, &f.Calls, &f.Retryable); err != nil {
			// Dropping a row silently would understate the failure count in a
			// view whose whole job is to report failures. Warn instead.
			slog.Warn("stats: failure summary row scan failed; skipping bucket", "err", err)
			continue
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
