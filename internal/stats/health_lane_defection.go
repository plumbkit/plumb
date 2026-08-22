package stats

// health_lane_defection.go — PLAN-368's first standing metric: the share of
// sessions where a plumb-read file was subsequently modified by something
// other than plumb, within the session, before plumb caught it.
//
// APPROXIMATION, documented rather than hidden. This reuses the SAME
// machinery that already answers "did this change under me?" for a live
// write — but review round 1 found the naive version of that reuse counts
// the WRONG thing twice over, so read this carefully before touching the
// query below.
//
// toolerror.KindUnreadOrStale is stamped on TWO distinct refusals
// (internal/tools/error_class.go): staleRead (ClassReRead) fires both when
// the caller never read the file at all (strict mode's read-before-write
// guard, internal/tools/strict_read.go) AND when expected_mtime/expected_sha
// mismatches; staleOverride (ClassPassForce) fires ONLY from write_file's
// default changedSinceSessionRead guard (internal/tools/write_guards.go) and
// undo_edit's changed-since-plumb's-own-write guard — and ONLY when the
// ReadTracker already held a recorded read that the on-disk mtime/sha then
// moved past. ClassReRead is therefore ambiguous (it also fires on a file
// NEVER read at all — not a defection, just strict mode doing its job);
// ClassPassForce is not. Restricting to write_file + ClassPassForce is what
// makes this specifically "a file this session read, then something else
// changed" rather than "any stale-write refusal" — undo_edit's guard compares
// against plumb's own WriteTracker, not a read, so it is excluded too.
//
// The denominator is scoped the same way: SessionsTotal counts sessions with
// at least one TRACKED READ call (read_file / read_symbol /
// read_multiple_files — the three tools that populate the ReadTracker), not
// every session that called any tool. A session that never read a file
// cannot have "a plumb-read file... subsequently modified", so counting it
// in the denominator would dilute the rate with sessions the metric was
// never about.
//
// What this STILL does not catch: a session that read a file, never
// attempted to write it again, and quietly moved on with the external edit
// unnoticed — the guard never fires because nothing ever tried the write it
// would have refused. That is real undercounting, not a bug: it is inherent
// in reusing a REFUSAL as the durable signal `calls` carries for this, and
// it is why this is a rate to trend rather than an absolute headcount — see
// the package doc on health.go.
import (
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// readTrackedTools are the tools that populate the per-session ReadTracker
// (internal/tools/read_tracker.go) — the set LaneDefectionForDay's
// denominator requires at least one call to, per session, before that
// session counts at all.
var readTrackedTools = []string{"read_file", "read_symbol", "read_multiple_files"}

// LaneDefectionDay is the lane-defection metric for one UTC day (and,
// optionally, one workspace).
type LaneDefectionDay struct {
	Day             string
	Workspace       string
	SessionsTotal   int64 // sessions with >=1 tracked read call (see readTrackedTools)
	SessionsFlagged int64
}

// Rate is SessionsFlagged / SessionsTotal, or 0 when no sessions read a file
// that day.
func (l LaneDefectionDay) Rate() float64 {
	if l.SessionsTotal == 0 {
		return 0
	}
	return float64(l.SessionsFlagged) / float64(l.SessionsTotal)
}

// LaneDefectionForDay computes the lane-defection metric for day (truncated
// to its UTC calendar date) and workspace (blank = every workspace). Cheap:
// two COUNT(DISTINCT session_id) queries over the called_at-indexed range,
// read-only over `calls`. See the file doc comment for exactly what is (and
// is not) counted on each side of the ratio.
func (d *DB) LaneDefectionForDay(day time.Time, workspace string) (LaneDefectionDay, error) {
	res := LaneDefectionDay{Day: day.UTC().Format("2006-01-02"), Workspace: workspace}
	if d == nil {
		return res, nil
	}
	from, to := utcDayBounds(day)
	where, args := dayWhere(from, to, workspace)

	readPlaceholders, readArgs := inClause(readTrackedTools)
	totalWhere := where + " AND tool IN (" + readPlaceholders + ")"
	total, err := d.countDistinctSessions(totalWhere, append(append([]any{}, args...), readArgs...))
	if err != nil {
		return res, err
	}
	res.SessionsTotal = total

	// write_file's default guard only, via KindUnreadOrStale+ClassPassForce —
	// see the file doc comment for why ClassReRead and undo_edit are excluded.
	flaggedWhere := where + " AND success = 0 AND tool = ? AND error_kind = ? AND remediation_class = ?"
	flaggedArgs := append(append([]any{}, args...), "write_file", string(toolerror.KindUnreadOrStale), string(toolerror.ClassPassForce))
	flagged, err := d.countDistinctSessions(flaggedWhere, flaggedArgs)
	if err != nil {
		return res, err
	}
	res.SessionsFlagged = flagged
	return res, nil
}

func (d *DB) countDistinctSessions(where string, args []any) (int64, error) {
	//nolint:gosec // G202: where is built by dayWhere/this file using ? placeholders only; no user values interpolated
	q := "SELECT COUNT(DISTINCT session_id) FROM tool_calls" + where
	var n int64
	if err := d.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("stats: lane defection: %w", err)
	}
	return n, nil
}

// inClause builds a "?,?,?" placeholder list for vals plus the matching
// bind-arg slice, for an "IN (...)" fragment appended to a WHERE built by
// dayWhere.
func inClause(vals []string) (string, []any) {
	placeholders := make([]byte, 0, len(vals)*2)
	args := make([]any, len(vals))
	for i, v := range vals {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args[i] = v
	}
	return string(placeholders), args
}
