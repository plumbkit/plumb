package stats

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CallSummary is the tool_calls metadata `plumb history show` prints beside a
// call's diffs. Its JSON keys are part of `plumb history show --json`'s output,
// so they follow that output's snake_case.
type CallSummary struct {
	Tool        string    `json:"tool"`
	ErrorMsg    string    `json:"error_msg,omitempty"`
	SessionName string    `json:"session_name,omitempty"`
	Workspace   string    `json:"workspace,omitempty"`
	CalledAt    time.Time `json:"called_at"`
	DurationMs  int64     `json:"duration_ms"`
	Success     bool      `json:"success"`
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
