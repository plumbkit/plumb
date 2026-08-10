package stats

// db_failures.go — the failure breakdown: how plumb's refusals and faults
// distribute across kinds, tools and client builds.

import (
	"fmt"
	"log/slog"

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

// FailureSummary returns the n busiest failure buckets matching filter, grouped
// by kind, tool and client build. n <= 0 applies defaultFailureBuckets.
//
// CLASSIFIED BUCKETS SORT FIRST, ahead of the unclassified one regardless of
// size. `tool_calls` is never pruned, so on any installation with history the
// pre-v14 failures outnumber the classified ones for a long time — sorting
// purely by count buries every actionable bucket under a row that says only
// "this predates the feature", and the LIMIT would then spend itself on it. The
// unclassified bucket is a known unknown, not a finding: it belongs last, where
// its note explains it.
//
// It is reported, never dropped: a report that silently omitted it would
// understate the failure count by exactly the amount it understands least.
func (d *DB) FailureSummary(n int, filter Filter) ([]FailureCount, error) {
	if d == nil {
		return nil, nil
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
	//nolint:gosec // G202: where is built by filter.where() using ? placeholders only; no user values interpolated
	q := `SELECT error_kind, tool, client_name, client_version,
	             COUNT(*) AS calls,
	             COALESCE(SUM(error_retryable), 0) AS retryable
	      FROM tool_calls` + where + `
	      GROUP BY error_kind, tool, client_name, client_version
	      ORDER BY (error_kind = '') ASC, calls DESC, error_kind, tool
	      LIMIT ?`
	args = append(args, n)

	rows, err := d.db.Query(q, args...)
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
