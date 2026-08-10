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

// FailureCount is one bucket of the failure breakdown.
//
// The grouping keys are chosen to be low-cardinality BY CONSTRUCTION rather
// than by hoping the data stays small: Kind is a closed thirteen-value set,
// Tool is the fixed registry, and the client name/version pair is one per
// client build in use. Nothing here groups by a value an agent supplies, so the
// result set cannot be blown up by a caller.
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

// FailureSummary returns failed calls matching filter, grouped by kind, tool
// and client build, busiest bucket first.
//
// Unclassified rows are reported, never dropped: a report that silently omitted
// them would understate the failure count by exactly the amount it understands
// least, which is the wrong way round.
func (d *DB) FailureSummary(filter Filter) ([]FailureCount, error) {
	if d == nil {
		return nil, nil
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
	      ORDER BY calls DESC, error_kind, tool`

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
