// Package stats records MCP tool call metrics to a SQLite database.
//
// The database lives at DataDir()/stats.db, which mirrors the same
// XDG_DATA_HOME convention used by the session package so all plumb
// data is co-located in one directory.
//
// WAL journal mode allows the daemon (writer) and the TUI / CLI (readers)
// to operate from different OS processes simultaneously without blocking.
package stats

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS tool_calls (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT    NOT NULL DEFAULT '',
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
CREATE INDEX IF NOT EXISTS idx_tc_workspace ON tool_calls(workspace);
CREATE INDEX IF NOT EXISTS idx_tc_session   ON tool_calls(session_id);
`

// migration describes a single forward schema step.
type migration struct {
	from, to int
	sql      string
}

// migrations is the ordered list of schema upgrades. Each entry carries the
// version it upgrades *from* and the version it produces. Apply in order.
var migrations = []migration{
	{from: 1, to: 2, sql: `ALTER TABLE tool_calls ADD COLUMN input_json  TEXT NOT NULL DEFAULT ''`},
	{from: 2, to: 3, sql: `ALTER TABLE tool_calls ADD COLUMN output_text TEXT NOT NULL DEFAULT ''`},
}

// migrate applies all pending forward migrations from currentVersion up to
// targetVersion. Errors are fatal — a half-applied migration is worse than a
// refused open.
func migrate(db *sql.DB, currentVersion, targetVersion int) error {
	for _, m := range migrations {
		if m.from < currentVersion || m.to > targetVersion {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			return fmt.Errorf("stats: migration v%d→v%d: %w", m.from, m.to, err)
		}
	}
	return nil
}

// DB is a thread-safe statistics store backed by SQLite.
type DB struct {
	db *sql.DB
	mu sync.Mutex
}

// DataDir returns the plumb data directory using XDG_DATA_HOME conventions,
// consistent with the session package.
func DataDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "plumb")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "plumb")
	}
	return filepath.Join(home, ".local", "share", "plumb")
}

// DBPath returns the legacy global stats database path. Kept for backward
// compatibility with installations that have history under the global path;
// new writes go to per-project databases via DBPathFor.
func DBPath() string {
	return filepath.Join(DataDir(), "stats.db")
}

// DBPathFor returns the per-workspace stats database path at
// <workspace>/.plumb/stats.db. This is the preferred location: stats live
// next to the project they describe and don't aggregate across unrelated
// codebases.
func DBPathFor(workspace string) string {
	return filepath.Join(workspace, ".plumb", "stats.db")
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
const SchemaVersion = 3

// Open opens (or creates) the stats database at path.
func Open(path string) (*DB, error) {
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

// CurrentSchemaVersion reads PRAGMA user_version from the open db. Returns
// 0 for pre-0.5.3 databases that were never stamped. Used by migrations.
func (d *DB) CurrentSchemaVersion() (int, error) {
	if d == nil {
		return 0, nil
	}
	var v int
	if err := d.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// OpenReadOnly opens an existing stats database for reading only.
// Returns (nil, nil) if the database does not yet exist.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	db, err := sql.Open("sqlite", path+"?mode=ro&_busy_timeout=1000")
	if err != nil {
		return nil, fmt.Errorf("stats: open readonly %s: %w", path, err)
	}
	return &DB{db: db}, nil
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
	Workspace   string
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

// Record inserts a call. Errors are silently dropped (stats are best-effort).
func (d *DB) Record(c Call) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	success := 1
	if !c.Success {
		success = 0
	}
	_, _ = d.db.Exec(
		`INSERT INTO tool_calls
		 (session_id, workspace, tool, called_at, duration_ms, input_bytes, output_bytes, success, error_msg, input_json, output_text)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.SessionID, c.Workspace, c.Tool,
		c.CalledAt.UnixMilli(), c.DurationMs,
		c.InputBytes, c.OutputBytes,
		success, c.ErrorMsg,
		capString(c.InputJSON), capString(c.OutputText),
	)
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

// Filter narrows a stats query. Empty fields are not constrained.
type Filter struct {
	Workspace string
	SessionID string
	Tool      string // when set, restricts to calls for this exact tool name
}

func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	if f.Workspace != "" {
		conds = append(conds, "workspace = ?")
		args = append(args, f.Workspace)
	}
	if f.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, f.SessionID)
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
	for i := range out {
		out[i].P95Ms = d.p95(filter, out[i].Tool)
		out[i].TokensSaved = d.tokensSavedFor(filter, out[i].Tool)
	}
	return out, nil
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

func (d *DB) p95(filter Filter, tool string) int64 {
	where, args := filter.where()
	var q string
	if where == "" {
		q = `SELECT duration_ms FROM tool_calls WHERE tool=? ORDER BY duration_ms`
		args = []any{tool}
	} else {
		q = `SELECT duration_ms FROM tool_calls` + where + ` AND tool=? ORDER BY duration_ms`
		args = append(args, tool)
	}
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var durations []int64
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err == nil {
			durations = append(durations, ms)
		}
	}
	if len(durations) == 0 {
		return 0
	}
	return durations[int(float64(len(durations)-1)*0.95)]
}

// RecentCall is a single recent invocation.
type RecentCall struct {
	Tool        string
	SessionID   string
	Workspace   string
	CalledAt    time.Time
	DurationMs  int64
	Success     bool
	ErrorMsg    string
	InputBytes  int
	OutputBytes int
	InputJSON   string // raw args JSON; empty for pre-v2 rows
	OutputText  string // full output; empty for pre-v3 rows
}

// Recent returns the n most recent calls matching filter.
func (d *DB) Recent(n int, filter Filter) ([]RecentCall, error) {
	if d == nil {
		return nil, nil
	}
	where, args := filter.where()
	q := `SELECT tool, session_id, workspace, called_at, duration_ms, success,
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
			&c.Tool, &c.SessionID, &c.Workspace, &calledMs, &c.DurationMs, &success,
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

// CallsForTool returns all recorded calls for a specific tool, workspace-wide,
// ordered newest-first. limit caps the result set (0 = no cap).
func (d *DB) CallsForTool(tool, workspace string, limit int) ([]RecentCall, error) {
	if d == nil {
		return nil, nil
	}
	f := Filter{Workspace: workspace, Tool: tool}
	if limit <= 0 {
		limit = 500
	}
	return d.Recent(limit, f)
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

// TotalTokensSaved sums TokensSaved across all matching calls. Best-effort
// estimate based on per-tool alternative-cost multipliers (see savings.go).
func (d *DB) TotalTokensSaved(filter Filter) int64 {
	if d == nil {
		return 0
	}
	where, args := filter.where()
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

// Workspaces returns all distinct workspaces that have recorded calls.
func (d *DB) Workspaces() ([]string, error) {
	if d == nil {
		return nil, nil
	}
	rows, err := d.db.Query(
		`SELECT DISTINCT workspace FROM tool_calls WHERE workspace != '' ORDER BY workspace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err == nil {
			out = append(out, w)
		}
	}
	return out, rows.Err()
}
