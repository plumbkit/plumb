package stats

// health_lane_defection.go — PLAN-368's first standing metric: the share of
// sessions where a plumb-read file was subsequently modified by something
// other than plumb, within the session, before plumb caught it.
//
// APPROXIMATION, documented rather than hidden — and review round 2 found
// round 1's fix had INVERTED the metric: restricting to write_file +
// ClassPassForce only counts a defection when the caller passed NO
// expected_mtime/expected_sha, which is the UNGUARDED overwrite path. A
// caller that follows the documented protocol (read_file → pass its mtime
// header as expected_mtime) hits verifyExpectedVersion's mismatch branch
// instead (write_guards.go), which classifies as ClassReRead, not
// ClassPassForce — so round 1's filter went to zero exactly as agents got
// BETTER at using plumb, the opposite of what a health metric should do.
// This is what "check the tool, not just the class" means below.
//
// toolerror.KindUnreadOrStale is stamped on TWO distinct refusals
// (internal/tools/error_class.go): staleRead (ClassReRead) fires when the
// caller never read the file at all (strict mode's read-before-write guard)
// OR when an explicit expected_mtime/expected_sha mismatches; staleOverride
// (ClassPassForce) fires only when the caller passed NEITHER guard and the
// session's OWN recorded read (ReadTracker) shows the file changed anyway.
// Both are genuine "read, then something else changed" defections — the
// bug was treating ClassReRead as inherently untrustworthy everywhere,
// when it only IS ambiguous on the tools that also have a "never read at
// all" refusal path.
//
// That path — requireStrictRead (internal/tools/strict_read.go) — is called
// from exactly two places (grep-verified): edit_file.go's checkStrictRead,
// and symbol_edits_apply.go's semanticStrictGate (rename_symbol,
// replace_symbol_body, insert_before_symbol, insert_after_symbol,
// safe_delete_symbol, move_symbol). Neither write_file nor transaction_apply
// ever calls it — grep for requireStrictRead's call sites confirms this, and
// neither tool has any OTHER route to a "never read" refusal either
// (write_file's only guards are the auto changedSinceSessionRead check and
// verifyExpectedVersion, both of which require the ReadTracker or the
// caller itself to already hold a read; transaction_apply's txValidateOp
// only checks expected_mtime/expected_sha when the operation carries one).
// So for THESE TWO TOOLS SPECIFICALLY, every KindUnreadOrStale row —
// ClassReRead or ClassPassForce, no further split needed — is unambiguously
// "this session read the file (or claimed to via expected_mtime/sha), then
// something else changed it". That is exactly what this metric counts.
//
// STILL EXCLUDED, and this is a real, disclosed undercount, not an
// oversight: edit_file and the six symbol-edit tools. edit_file DOES also
// have an unambiguous expected_mtime/expected_sha mismatch path
// (checkExpectedVersion, the same verifyExpectedVersion write_file uses),
// but `calls` has no column recording whether a given row's args included
// expected_mtime/expected_sha (only the possibly-truncated input_json blob),
// so a ClassReRead row from edit_file cannot be split from its
// requireStrictRead-sourced "never read at all" rows without parsing that
// JSON — not attempted here. The six symbol-edit tools have NO unambiguous
// sub-case at all: requireStrictRead is their ONLY refusal source, so every
// row they produce genuinely conflates the two meanings. undo_edit is
// excluded on different grounds — its guard compares against plumb's OWN
// WriteTracker (did plumb's last write survive), not a read, so it isn't
// this metric's signal at all.
//
// The denominator is scoped the same way: SessionsTotal counts sessions with
// at least one TRACKED READ call (read_file / read_symbol /
// read_multiple_files — the three tools that populate the ReadTracker), not
// every session that called any tool. A session that never read a file
// cannot have "a plumb-read file... subsequently modified", so counting it
// in the denominator would dilute the rate with sessions the metric was
// never about.
//
// What this STILL does not catch, beyond the edit_file/symbol-tool
// undercount above: a session that read a file, never attempted to write it
// again (through ANY tool), and quietly moved on with the external edit
// unnoticed — no guard ever fires because nothing ever tried the write it
// would have refused. That is inherent in reusing a REFUSAL as the durable
// signal `calls` carries for this, and it is why this is a rate to trend
// rather than an absolute headcount — see the package doc on health.go.
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

// unambiguousDefectionTools are the write tools whose KindUnreadOrStale
// refusals can ONLY mean "read (or claimed via expected_mtime/sha), then
// changed" — never "never read at all". See the file doc comment for the
// grep-verified reasoning (requireStrictRead is never called from either).
var unambiguousDefectionTools = []string{"write_file", "transaction_apply"}

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

	// write_file + transaction_apply only, any KindUnreadOrStale refusal
	// (ClassReRead covers the guarded expected_mtime/expected_sha path;
	// ClassPassForce covers the unguarded auto-detect path) — see the file
	// doc comment for why these two tools' rows need no further split, and
	// why edit_file/the symbol-edit tools cannot safely be added the same way.
	toolPlaceholders, toolArgs := inClause(unambiguousDefectionTools)
	flaggedWhere := where + " AND success = 0 AND tool IN (" + toolPlaceholders + ") AND error_kind = ?"
	flaggedArgs := append(append(append([]any{}, args...), toolArgs...), string(toolerror.KindUnreadOrStale))
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
