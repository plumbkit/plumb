package stats

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ToolStat summarises calls for one tool.
type ToolStat struct {
	Tool          string
	Calls         int64
	AvgMs         float64
	P95Ms         int64
	TotalInputKB  float64
	TotalOutputKB float64
	Errors        int64
	TokensSaved   int64
	LastCalledAt  time.Time
}

// ActivitySummary is a bucketed view of recent tool-call activity.
type ActivitySummary struct {
	Window  time.Duration
	Calls   int64
	Buckets []int64
}

// Filter narrows a stats query. Empty fields are not constrained.
type Filter struct {
	SessionID   string
	SessionName string    // when set, restricts to calls for this session name
	Workspace   string    // absolute path; when set, restricts to calls for this workspace
	Tool        string    // when set, restricts to calls for this exact tool name
	Since       time.Time // when set, restricts to calls at or after this time
}

func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	if f.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.SessionName != "" {
		conds = append(conds, "session_name = ?")
		args = append(args, f.SessionName)
	}
	if f.Workspace != "" {
		conds = append(conds, "workspace = ?")
		args = append(args, f.Workspace)
	}
	if f.Tool != "" {
		conds = append(conds, "tool = ?")
		args = append(args, f.Tool)
	}
	if !f.Since.IsZero() {
		conds = append(conds, "called_at >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// Summary returns per-tool stats matching filter. Empty filter = all rows.
// p95 and TokensSaved are computed in a single additional query each (not per-tool).
func (d *DB) Summary(filter Filter) ([]ToolStat, error) {
	if d == nil {
		return nil, nil
	}
	where, args := filter.where()
	// summaryBase is a compile-time constant; where is built by filter.where() using
	// ? placeholders only — no user values are ever interpolated into the SQL string.
	summaryBase := `SELECT tool,
		         COUNT(*) AS calls,
		         COALESCE(AVG(duration_ms), 0) AS avg_ms,
		         COALESCE(SUM(input_bytes), 0) AS total_in,
		         COALESCE(SUM(output_bytes), 0) AS total_out,
		         SUM(CASE WHEN success=0 THEN 1 ELSE 0 END) AS errors,
		         MAX(called_at) AS last_called
		  FROM tool_calls`
	q := summaryBase + where + " GROUP BY tool ORDER BY calls DESC" //nolint:gosec // G202: see comment above

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("stats: summary: %w", err)
	}
	defer rows.Close()

	var out []ToolStat
	for rows.Next() {
		var s ToolStat
		var lastMs int64
		var totalIn, totalOut int64
		if err := rows.Scan(&s.Tool, &s.Calls, &s.AvgMs, &totalIn, &totalOut, &s.Errors, &lastMs); err != nil {
			continue
		}
		s.TotalInputKB = float64(totalIn) / 1024
		s.TotalOutputKB = float64(totalOut) / 1024
		s.LastCalledAt = time.UnixMilli(lastMs)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Compute p95 per tool in one query, then join in Go.
	p95map := d.p95All(filter)
	for i := range out {
		out[i].P95Ms = p95map[out[i].Tool]
		out[i].TokensSaved = d.tokensSavedFor(filter, out[i].Tool)
	}
	return out, nil
}

// Activity returns a bucketed activity summary for calls in the last window.
func (d *DB) Activity(window time.Duration, bucketCount int, filter Filter) (ActivitySummary, error) {
	return d.ActivityAt(time.Now(), window, bucketCount, filter)
}

// ActivityAt returns a bucketed activity summary ending at now. It exists so
// tests can use stable timestamps while the TUI uses Activity.
func (d *DB) ActivityAt(now time.Time, window time.Duration, bucketCount int, filter Filter) (ActivitySummary, error) {
	if d == nil {
		return ActivitySummary{}, nil
	}
	if window <= 0 {
		window = time.Minute
	}
	if bucketCount <= 0 {
		bucketCount = 16
	}
	start := now.Add(-window)
	summary := ActivitySummary{
		Window:  window,
		Buckets: make([]int64, bucketCount),
	}

	where, args := filter.where()
	if where == "" {
		where = " WHERE called_at >= ? AND called_at <= ?"
	} else {
		where += " AND called_at >= ? AND called_at <= ?"
	}
	args = append(args, start.UnixMilli(), now.UnixMilli())
	// where is built by filter.where() using ? placeholders; no user values interpolated.
	q := `SELECT called_at FROM tool_calls` + where + ` ORDER BY called_at` //nolint:gosec // G202: see comment above
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return summary, fmt.Errorf("stats: activity: %w", err)
	}
	defer rows.Close()

	bucketMs := max(window.Milliseconds()/int64(bucketCount), 1)
	startMs := start.UnixMilli()
	for rows.Next() {
		var calledMs int64
		if err := rows.Scan(&calledMs); err != nil {
			continue
		}
		idx := max(int((calledMs-startMs)/bucketMs), 0)
		if idx >= bucketCount {
			idx = bucketCount - 1
		}
		summary.Buckets[idx]++
		summary.Calls++
	}
	return summary, rows.Err()
}

// FirstCallAt returns the timestamp of the earliest recorded call, or zero if empty.
func (d *DB) FirstCallAt() time.Time {
	if d == nil {
		return time.Time{}
	}
	var ms sql.NullInt64
	err := d.db.QueryRow("SELECT MIN(called_at) FROM tool_calls").Scan(&ms)
	if err != nil || !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64)
}

// tokensSavedFor totals estimated savings for one tool under filter.
func (d *DB) tokensSavedFor(filter Filter, tool string) int64 {
	if !HasSavingsModel(tool) {
		return 0
	}
	where, args := filter.where()
	var q string
	if where == "" {
		q = `SELECT output_bytes, COALESCE(client_name, ''), tokens_saved, savings_model_version FROM tool_calls WHERE tool=?`
		args = []any{tool}
	} else {
		q = `SELECT output_bytes, COALESCE(client_name, ''), tokens_saved, savings_model_version FROM tool_calls` + where + ` AND tool=?` //nolint:gosec // G202: where built from filter.where() using ? placeholders only
		args = append(args, tool)
	}
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var out, modelVersion int
		var clientName string
		var stored int64
		if err := rows.Scan(&out, &clientName, &stored, &modelVersion); err == nil {
			total += savingsForRow(tool, clientName, out, stored, modelVersion)
		}
	}
	return total
}

// savingsForRow returns the savings credited to one tool_calls row. A row scored
// at write time (savings_model_version > 0) is trusted as stored — provenance over
// recompute; an unscored legacy row (version 0) is recomputed under the profile
// model so historical totals stay populated until that legacy data is retired.
func savingsForRow(tool, clientName string, outputBytes int, stored int64, modelVersion int) int64 {
	if modelVersion > 0 {
		return stored
	}
	return int64(TokensSavedForClient(tool, clientName, outputBytes))
}

// p95All fetches duration_ms for all rows matching filter in a single query
// and computes the p95 per tool in Go. One round-trip regardless of how many
// distinct tools exist — replaces the old per-tool p95() loop.
func (d *DB) p95All(filter Filter) map[string]int64 {
	where, args := filter.where()
	// where is built by filter.where() using ? placeholders; no user values interpolated.
	q := `SELECT tool, duration_ms FROM tool_calls` + where + ` ORDER BY tool, duration_ms` //nolint:gosec // G202: see comment above
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	// Accumulate durations per tool (already sorted by tool then duration_ms).
	type toolDurs struct {
		tool string
		durs []int64
	}
	var groups []toolDurs
	var cur *toolDurs
	for rows.Next() {
		var tool string
		var ms int64
		if err := rows.Scan(&tool, &ms); err != nil {
			continue
		}
		if cur == nil || cur.tool != tool {
			groups = append(groups, toolDurs{tool: tool})
			cur = &groups[len(groups)-1]
		}
		cur.durs = append(cur.durs, ms)
	}

	out := make(map[string]int64, len(groups))
	for _, g := range groups {
		if len(g.durs) == 0 {
			continue
		}
		idx := int(float64(len(g.durs)-1) * 0.95)
		out[g.tool] = g.durs[idx]
	}
	return out
}

// RecentCall is a single recent invocation.
type RecentCall struct {
	Tool        string
	SessionID   string
	SessionName string // human-readable name
	Workspace   string // absolute path to the project root
	CalledAt    time.Time
	DurationMs  int64
	Success     bool
	ErrorMsg    string
	InputBytes  int
	OutputBytes int
	InputJSON   string // raw args JSON
	OutputText  string // full output
}

// Recent returns the n most recent calls matching filter.
func (d *DB) Recent(n int, filter Filter) ([]RecentCall, error) {
	if d == nil {
		return nil, nil
	}
	where, args := filter.where()
	//nolint:gosec // G202: where is built by filter.where() using ? placeholders only; no user values interpolated
	q := `SELECT tool, session_id, session_name, workspace, called_at, duration_ms, success,
	             error_msg, input_bytes, output_bytes, input_json, output_text
	      FROM tool_calls` + where + ` ORDER BY called_at DESC LIMIT ?`
	args = append(args, n)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("stats: recent: %w", err)
	}
	defer rows.Close()

	var out []RecentCall
	for rows.Next() {
		var c RecentCall
		var calledMs int64
		var success int
		if err := rows.Scan(
			&c.Tool, &c.SessionID, &c.SessionName, &c.Workspace, &calledMs, &c.DurationMs, &success,
			&c.ErrorMsg, &c.InputBytes, &c.OutputBytes, &c.InputJSON, &c.OutputText,
		); err != nil {
			continue
		}
		c.CalledAt = time.UnixMilli(calledMs)
		c.Success = success == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// Slowest returns the n slowest calls matching filter, ordered by duration
// descending. Backs daemon_info's per-session "slowest calls" view. Like
// CallsForTool it omits the large input_json / output_text columns.
func (d *DB) Slowest(n int, filter Filter) ([]RecentCall, error) {
	if d == nil {
		return nil, nil
	}
	where, args := filter.where()
	//nolint:gosec // G202: where is built by filter.where() using ? placeholders only; no user values interpolated
	q := `SELECT tool, session_id, session_name, workspace, called_at, duration_ms, success,
	             error_msg, input_bytes, output_bytes
	      FROM tool_calls` + where + ` ORDER BY duration_ms DESC LIMIT ?`
	args = append(args, n)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("stats: slowest: %w", err)
	}
	defer rows.Close()

	var out []RecentCall
	for rows.Next() {
		var c RecentCall
		var calledMs int64
		var success int
		if err := rows.Scan(
			&c.Tool, &c.SessionID, &c.SessionName, &c.Workspace, &calledMs, &c.DurationMs, &success,
			&c.ErrorMsg, &c.InputBytes, &c.OutputBytes,
		); err != nil {
			continue
		}
		c.CalledAt = time.UnixMilli(calledMs)
		c.Success = success == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// CallsForTool returns recorded calls for a specific tool in this database,
// ordered newest-first. limit caps the result set (0 = no cap).
// input_json and output_text (potentially 64 KiB each) are intentionally
// excluded from this list query — they are fetched on demand via CallDetail.
func (d *DB) CallsForTool(tool string, workspace string, limit int) ([]RecentCall, error) {
	if d == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	f := Filter{Tool: tool, Workspace: workspace}
	where, args := f.where()
	//nolint:gosec // G202: where is built by filter.where() using ? placeholders only; no user values interpolated
	q := `SELECT tool, session_id, session_name, workspace, called_at, duration_ms, success,
	             error_msg, input_bytes, output_bytes
	      FROM tool_calls` + where + ` ORDER BY called_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("stats: calls for tool: %w", err)
	}
	defer rows.Close()

	var out []RecentCall
	for rows.Next() {
		var c RecentCall
		var calledMs int64
		var success int
		if err := rows.Scan(
			&c.Tool, &c.SessionID, &c.SessionName, &c.Workspace, &calledMs, &c.DurationMs, &success,
			&c.ErrorMsg, &c.InputBytes, &c.OutputBytes,
		); err != nil {
			continue
		}
		c.CalledAt = time.UnixMilli(calledMs)
		c.Success = success == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecentWritesByWorkspace returns the most recent mutating tool calls on a
// workspace, newest-first, restricted to the given tool names. It backs the
// workspace_sessions "recent writes" feed: an agent uses it to spot files a
// co-worker session touched recently and re-read them before editing.
//
// Only the small columns needed for the feed are selected (input_json carries
// the file path); output_text is deliberately omitted. The tool-name set is
// passed in (the tools package owns the write-tool list) and rendered as a
// parameterised IN clause, so no caller value is ever interpolated. Returns nil
// when writeTools is empty or d is nil.
func (d *DB) RecentWritesByWorkspace(workspace string, writeTools []string, limit int) ([]RecentCall, error) {
	if d == nil || len(writeTools) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(writeTools)), ",")
	args := make([]any, 0, len(writeTools)+2)
	args = append(args, workspace)
	for _, t := range writeTools {
		args = append(args, t)
	}
	args = append(args, limit)
	//nolint:gosec // G202: placeholders is only "?,?,.."; every value is a bound arg
	q := `SELECT tool, session_id, session_name, workspace, called_at, duration_ms, success,
	             error_msg, input_bytes, output_bytes, input_json, ''
	      FROM tool_calls
	      WHERE workspace = ? AND tool IN (` + placeholders + `)
	      ORDER BY called_at DESC LIMIT ?`

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("stats: recent writes: %w", err)
	}
	defer rows.Close()

	var out []RecentCall
	for rows.Next() {
		var c RecentCall
		var calledMs int64
		var success int
		if err := rows.Scan(
			&c.Tool, &c.SessionID, &c.SessionName, &c.Workspace, &calledMs, &c.DurationMs, &success,
			&c.ErrorMsg, &c.InputBytes, &c.OutputBytes, &c.InputJSON, &c.OutputText,
		); err != nil {
			continue
		}
		c.CalledAt = time.UnixMilli(calledMs)
		c.Success = success == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// CallDetail fetches the full input_json and output_text for a single call
// identified by (workspace, session_id, called_at). Returns empty strings if
// not found.
func (d *DB) CallDetail(workspace, sessionID string, calledAt time.Time) (inputJSON, outputText string) {
	if d == nil {
		return
	}
	_ = d.db.QueryRow(
		`SELECT input_json, output_text FROM tool_calls WHERE workspace=? AND session_id=? AND called_at=? LIMIT 1`,
		workspace, sessionID, calledAt.UnixMilli(),
	).Scan(&inputJSON, &outputText)
	return
}

// TotalCalls returns the total number of recorded calls matching filter.
func (d *DB) TotalCalls(filter Filter) int64 {
	if d == nil {
		return 0
	}
	where, args := filter.where()
	q := `SELECT COUNT(*) FROM tool_calls` + where
	var n int64
	_ = d.db.QueryRow(q, args...).Scan(&n)
	return n
}

// TotalSessions returns the number of distinct recorded sessions matching filter.
func (d *DB) TotalSessions(filter Filter) int64 {
	if d == nil {
		return 0
	}
	where, args := filter.where()
	q := `SELECT COUNT(DISTINCT session_id) FROM tool_calls` + where
	var n int64
	_ = d.db.QueryRow(q, args...).Scan(&n)
	return n
}

// TotalTokensSaved sums TokensSaved across all matching calls. Best-effort
// estimate based on per-tool alternative-cost multipliers (see savings.go).
func (d *DB) TotalTokensSaved(filter Filter) int64 {
	return d.TotalTokensSavedSince(time.Time{}, filter)
}

// TotalTokensSavedSince sums TokensSaved across matching calls recorded at or
// after since. A zero since includes all matching rows.
func (d *DB) TotalTokensSavedSince(since time.Time, filter Filter) int64 {
	if d == nil {
		return 0
	}
	where, args := filter.where()
	if !since.IsZero() {
		if where == "" {
			where = " WHERE called_at >= ?"
		} else {
			where += " AND called_at >= ?"
		}
		args = append(args, since.UnixMilli())
	}
	// where is built by filter.where() using ? placeholders; no user values interpolated.
	q := `SELECT tool, output_bytes, COALESCE(client_name, ''), tokens_saved, savings_model_version FROM tool_calls` + where //nolint:gosec // G202: see comment above
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var tool, clientName string
		var out, modelVersion int
		var stored int64
		if err := rows.Scan(&tool, &out, &clientName, &stored, &modelVersion); err != nil {
			continue
		}
		total += savingsForRow(tool, clientName, out, stored, modelVersion)
	}
	return total
}
