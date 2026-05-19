// Package stats records MCP tool call metrics to a SQLite database.
//
// The database lives in plumb's global data directory. Each row records the
// workspace and session it belongs to, matching plumb's single-daemon model.
//
// WAL journal mode allows the daemon (writer) and the TUI / CLI (readers)
// to operate from different OS processes simultaneously without blocking.
package stats

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/golimpio/plumb/internal/config"
)

// schema is the current fresh database shape. The global stats database uses
// row-level workspace and session fields to separate project history inside the
// single daemon-owned store.
const schema = `
CREATE TABLE IF NOT EXISTS tool_calls (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT    NOT NULL DEFAULT '',
    session_name TEXT    NOT NULL DEFAULT '',
    workspace    TEXT    NOT NULL DEFAULT '',
    tool         TEXT    NOT NULL,
    called_at    INTEGER NOT NULL,
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    input_bytes  INTEGER NOT NULL DEFAULT 0,
    output_bytes INTEGER NOT NULL DEFAULT 0,
    success      INTEGER NOT NULL DEFAULT 1,
    error_msg    TEXT    NOT NULL DEFAULT '',
    input_json   TEXT    NOT NULL DEFAULT '',
    output_text  TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tc_tool      ON tool_calls(tool);
CREATE INDEX IF NOT EXISTS idx_tc_called_at ON tool_calls(called_at);
CREATE INDEX IF NOT EXISTS idx_tc_session   ON tool_calls(session_id);
CREATE INDEX IF NOT EXISTS idx_tc_workspace ON tool_calls(workspace);
CREATE INDEX IF NOT EXISTS idx_tc_ws_session ON tool_calls(workspace, session_id);
`

// migration describes a single forward schema step. For ADD COLUMN migrations,
// addColumn names the column being added so we can skip the step if it
// already exists (recovering from databases stamped by older buggy builds
// that created the column up-front).
type migration struct {
	from, to  int
	addColumn string
	sql       string
}

// migrations is the ordered list of schema upgrades. Each entry carries the
// version it upgrades *from* and the version it produces. Apply in order.
var migrations = []migration{
	{from: 1, to: 2, addColumn: "input_json", sql: `ALTER TABLE tool_calls ADD COLUMN input_json    TEXT NOT NULL DEFAULT ''`},
	{from: 2, to: 3, addColumn: "output_text", sql: `ALTER TABLE tool_calls ADD COLUMN output_text  TEXT NOT NULL DEFAULT ''`},
	{from: 3, to: 4, addColumn: "session_name", sql: `ALTER TABLE tool_calls ADD COLUMN session_name TEXT NOT NULL DEFAULT ''`},
	{from: 4, to: 5, addColumn: "workspace", sql: `ALTER TABLE tool_calls ADD COLUMN workspace    TEXT NOT NULL DEFAULT ''`},
}

// ErrReadOnlySchemaUpgradeRequired marks a stats database that is too old for
// read-only query paths. Open it read-write through Open to apply migrations.
var ErrReadOnlySchemaUpgradeRequired = errors.New("stats schema upgrade required")

// migrate applies all pending forward migrations from currentVersion up to
// targetVersion. ADD COLUMN steps are skipped when the column already exists,
// which keeps the path idempotent in two cases: (a) an unstamped database
// created by a build that defined the column in the baseline schema; (b)
// re-running migrate after a partial earlier run.
func migrate(db *sql.DB, currentVersion, targetVersion int) error {
	for _, m := range migrations {
		if m.from < currentVersion || m.to > targetVersion {
			continue
		}
		if m.addColumn != "" {
			has, err := hasColumn(db, "tool_calls", m.addColumn)
			if err != nil {
				return fmt.Errorf("stats: migration v%d→v%d: check column: %w", m.from, m.to, err)
			}
			if has {
				continue
			}
		}
		if _, err := db.Exec(m.sql); err != nil {
			return fmt.Errorf("stats: migration v%d→v%d: %w", m.from, m.to, err)
		}
	}
	return nil
}

// hasColumn reports whether table has a column named col, via PRAGMA
// table_info. Used to make ADD COLUMN migrations idempotent.
func hasColumn(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// DB is a thread-safe statistics store backed by SQLite.
type DB struct {
	db *sql.DB
	mu sync.Mutex
}

// DBPathFor returns the global stats database path in the persistent data
// directory.
func DBPathFor() string {
	return filepath.Join(config.DataDir(), "stats.db")
}

// SchemaVersion is the current on-disk stats schema version. Persisted in
// PRAGMA user_version on every Open. Open reads the on-disk version, applies
// any pending migrations, then stamps the new version.
//
// History:
//
//	0 — pre-versioned (everything up to 0.5.2)
//	1 — first explicitly versioned schema (0.5.3+) — no column changes
//	2 — added input_json column (0.5.12+)
//	3 — added output_text column (0.5.12+)
//	4 — added session_name column (0.5.30+)
//	5 — added workspace column (0.5.31+)
const SchemaVersion = 5

// Open opens (or creates) the stats database at the conventional global path.
func Open() (*DB, error) {
	path := DBPathFor()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("stats: mkdir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("stats: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("stats: schema: %w", err)
	}
	// Read the current schema version and apply any pending migrations.
	var currentVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&currentVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("stats: reading user_version: %w", err)
	}
	if currentVersion < SchemaVersion {
		if err := migrate(db, currentVersion, SchemaVersion); err != nil {
			db.Close()
			return nil, err
		}
	}
	// Stamp the (possibly updated) schema version.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		db.Close()
		return nil, fmt.Errorf("stats: stamping user_version: %w", err)
	}
	return &DB{db: db}, nil
}

// OpenReadOnly opens the existing global stats database for reading only.
// Returns (nil, nil) if the database does not yet exist.
func OpenReadOnly() (*DB, error) {
	path := DBPathFor()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	db, err := sql.Open("sqlite", path+"?mode=ro&_busy_timeout=1000")
	if err != nil {
		return nil, fmt.Errorf("stats: open readonly %s: %w", path, err)
	}
	if err := checkReadOnlySchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%w; delete %s so plumb can create a fresh global stats database", err, path)
	}
	return &DB{db: db}, nil
}

func checkReadOnlySchema(db *sql.DB) error {
	var currentVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&currentVersion); err != nil {
		return fmt.Errorf("stats: reading readonly schema version: %w", err)
	}
	if currentVersion >= SchemaVersion {
		return nil
	}
	return fmt.Errorf("%w: stats database is schema version %d, current version is %d", ErrReadOnlySchemaUpgradeRequired, currentVersion, SchemaVersion)
}

// Close closes the database.
func (d *DB) Close() {
	if d != nil {
		_ = d.db.Close()
	}
}

// Call holds one tool invocation record.
type Call struct {
	SessionID   string
	SessionName string // human-readable name, e.g. "SWIFT-FALCON"
	Workspace   string // absolute path to the project root
	Tool        string
	CalledAt    time.Time
	DurationMs  int64
	InputBytes  int
	OutputBytes int
	Success     bool
	ErrorMsg    string
	InputJSON   string // raw JSON args as sent to the tool (capped at 64 KiB)
	OutputText  string // full tool output (capped at 64 KiB)
}

// maxStoredBytes caps the size of input_json and output_text stored per call.
// Large tool outputs (e.g. search_in_files on a big repo) are truncated to
// keep the DB compact. 64 KiB is generous for debugging purposes.
const maxStoredBytes = 64 * 1024

func capString(s string) string {
	if len(s) > maxStoredBytes {
		return s[:maxStoredBytes]
	}
	return s
}

// Record inserts a call. Stats are best-effort, but the caller gets the
// insert error so the daemon can log storage failures.
func (d *DB) Record(c Call) error {
	if d == nil {
		return nil
	}
	if c.Workspace == "" {
		return fmt.Errorf("stats: workspace is required")
	}
	if c.SessionID == "" {
		return fmt.Errorf("stats: session_id is required")
	}
	if c.Tool == "" {
		return fmt.Errorf("stats: tool is required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	success := 1
	if !c.Success {
		success = 0
	}
	if _, err := d.db.Exec(
		`INSERT INTO tool_calls
		 (session_id, session_name, workspace, tool, called_at, duration_ms, input_bytes, output_bytes, success, error_msg, input_json, output_text)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.SessionID, c.SessionName, c.Workspace, c.Tool,
		c.CalledAt.UnixMilli(), c.DurationMs,
		c.InputBytes, c.OutputBytes,
		success, c.ErrorMsg,
		capString(c.InputJSON), capString(c.OutputText),
	); err != nil {
		return fmt.Errorf("stats: insert call: %w", err)
	}
	return nil
}

// RenameSession updates the stored human-readable name for all calls in a
// session. It is best-effort for the global stats database.
func (d *DB) RenameSession(sessionID, name string) error {
	if d == nil || sessionID == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.db.Exec(`UPDATE tool_calls SET session_name=? WHERE session_id=?`, name, sessionID); err != nil {
		return fmt.Errorf("stats: rename session: %w", err)
	}
	return nil
}

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
	SessionName string // when set, restricts to calls for this session name
	Workspace   string // absolute path; when set, restricts to calls for this workspace
	Tool        string // when set, restricts to calls for this exact tool name
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
	q := `SELECT tool,
		         COUNT(*) AS calls,
		         COALESCE(AVG(duration_ms), 0) AS avg_ms,
		         COALESCE(SUM(input_bytes), 0) AS total_in,
		         COALESCE(SUM(output_bytes), 0) AS total_out,
		         SUM(CASE WHEN success=0 THEN 1 ELSE 0 END) AS errors,
		         MAX(called_at) AS last_called
		  FROM tool_calls` + where + " GROUP BY tool ORDER BY calls DESC"

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
	q := `SELECT called_at FROM tool_calls` + where + ` ORDER BY called_at`
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
		q = `SELECT output_bytes FROM tool_calls WHERE tool=?`
		args = []any{tool}
	} else {
		q = `SELECT output_bytes FROM tool_calls` + where + ` AND tool=?`
		args = append(args, tool)
	}
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var out int
		if err := rows.Scan(&out); err == nil {
			total += int64(TokensSaved(tool, out))
		}
	}
	return total
}

// p95All fetches duration_ms for all rows matching filter in a single query
// and computes the p95 per tool in Go. One round-trip regardless of how many
// distinct tools exist — replaces the old per-tool p95() loop.
func (d *DB) p95All(filter Filter) map[string]int64 {
	where, args := filter.where()
	q := `SELECT tool, duration_ms FROM tool_calls` + where + ` ORDER BY tool, duration_ms`
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
	q := `SELECT tool, output_bytes FROM tool_calls` + where
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var tool string
		var out int
		if err := rows.Scan(&tool, &out); err != nil {
			continue
		}
		total += int64(TokensSaved(tool, out))
	}
	return total
}
