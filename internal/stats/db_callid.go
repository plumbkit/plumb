package stats

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CallSummary is the tool_calls metadata `plumb history show` prints beside a
// call's diffs.
type CallSummary struct {
	Tool, ErrorMsg, SessionName, Workspace string
	CalledAt                               time.Time
	DurationMs                             int64
	Success                                bool
}

// CallByID returns the call recorded under callID. ok is false when no row
// carries it (unknown id, or a row from before v20).
func (d *DB) CallByID(callID string) (CallSummary, bool, error) {
	if d == nil || callID == "" {
		return CallSummary{}, false, nil
	}
	var s CallSummary
	var at int64
	var success int
	err := d.db.QueryRow(`SELECT tool, called_at, duration_ms, success, error_msg, session_name, workspace
		FROM tool_calls WHERE call_id = ? ORDER BY id DESC LIMIT 1`, callID).
		Scan(&s.Tool, &at, &s.DurationMs, &success, &s.ErrorMsg, &s.SessionName, &s.Workspace)
	if errors.Is(err, sql.ErrNoRows) {
		return CallSummary{}, false, nil
	}
	if err != nil {
		return CallSummary{}, false, fmt.Errorf("stats: call by id: %w", err)
	}
	s.CalledAt, s.Success = time.UnixMilli(at), success != 0
	return s, true, nil
}
