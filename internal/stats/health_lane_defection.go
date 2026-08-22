package stats

// health_lane_defection.go — PLAN-368's first standing metric: the share of
// sessions where a plumb-read file was subsequently modified by something
// other than plumb, within the session, before plumb caught it.
//
// APPROXIMATION, documented rather than hidden. This reuses the SAME
// machinery that already answers "did this change under me?" for a live
// write: toolerror.KindUnreadOrStale, stamped on every write plumb refused
// because changedSinceSessionRead (internal/tools/write_guards.go) or the
// strict-mode read-before-write guard (internal/tools/strict_read.go) found
// the on-disk mtime/sha had moved past what the session's ReadTracker
// recorded at read time — comparing the read-tracker's record against the
// file's mtime is exactly what that guard does. A session is flagged the
// moment ONE such refusal lands, whatever the agent does next (retry, give
// up, ask a human): the guard has already proved the defection happened, and
// this metric only counts how many sessions hit it.
//
// What it does NOT catch: a session that read a file, never attempted to
// write it again, and quietly moved on with the external edit unnoticed —
// the guard never fires because nothing ever tried the write it would have
// refused. That is real undercounting, not a bug: it is inherent in reusing
// a REFUSAL as the durable signal `calls` carries for this, and it is why
// this is a rate to trend rather than an absolute headcount — see the
// package doc on health.go.
import (
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// LaneDefectionDay is the lane-defection metric for one UTC day (and,
// optionally, one workspace).
type LaneDefectionDay struct {
	Day             string
	Workspace       string
	SessionsTotal   int64
	SessionsFlagged int64
}

// Rate is SessionsFlagged / SessionsTotal, or 0 when no sessions were active
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
// read-only over `calls`.
func (d *DB) LaneDefectionForDay(day time.Time, workspace string) (LaneDefectionDay, error) {
	res := LaneDefectionDay{Day: day.UTC().Format("2006-01-02"), Workspace: workspace}
	if d == nil {
		return res, nil
	}
	from, to := utcDayBounds(day)
	where, args := dayWhere(from, to, workspace)

	total, err := d.countDistinctSessions(where, args)
	if err != nil {
		return res, err
	}
	res.SessionsTotal = total

	flaggedWhere := where + " AND success = 0 AND error_kind = ?"
	flaggedArgs := append(append([]any{}, args...), string(toolerror.KindUnreadOrStale))
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
