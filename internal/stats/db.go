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
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/plumbkit/plumb/internal/config"
)

// schema is the current fresh database shape. The global stats database uses
// row-level workspace and session fields to separate project history inside the
// single daemon-owned store.
const schema = `
CREATE TABLE IF NOT EXISTS tool_calls (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id     TEXT    NOT NULL DEFAULT '',
    session_name   TEXT    NOT NULL DEFAULT '',
    workspace      TEXT    NOT NULL DEFAULT '',
    tool           TEXT    NOT NULL,
    called_at      INTEGER NOT NULL,
    duration_ms    INTEGER NOT NULL DEFAULT 0,
    input_bytes    INTEGER NOT NULL DEFAULT 0,
    output_bytes   INTEGER NOT NULL DEFAULT 0,
    success        INTEGER NOT NULL DEFAULT 1,
    error_msg      TEXT    NOT NULL DEFAULT '',
    input_json     TEXT    NOT NULL DEFAULT '',
    output_text    TEXT    NOT NULL DEFAULT '',
    client_name    TEXT    NOT NULL DEFAULT '',
    client_version TEXT    NOT NULL DEFAULT ''
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
	{from: 5, to: 6, addColumn: "client_name", sql: `ALTER TABLE tool_calls ADD COLUMN client_name    TEXT NOT NULL DEFAULT ''`},
	{from: 6, to: 7, addColumn: "client_version", sql: `ALTER TABLE tool_calls ADD COLUMN client_version TEXT NOT NULL DEFAULT ''`},
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
//	6 — added client_name column (0.7.6+)
//	7 — added client_version column (0.7.6+)
const SchemaVersion = 7

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
	// synchronous=NORMAL is corruption-safe under WAL and avoids an fsync per
	// commit; the only exposure is losing the last batch of stats on a hard
	// power cut, which is acceptable for metrics. WAL + busy_timeout come from
	// the DSN; assert NORMAL here since it is per-connection.
	if _, err := db.Exec("PRAGMA synchronous = NORMAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("stats: synchronous: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("stats: schema: %w", err)
	}
	// Read the current schema version and apply any pending migrations. The
	// user_version stamp is a write, so only issue it when the version actually
	// moved — stamping on every Open turns a read-only open into a writer that
	// contends for the write lock.
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
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
			db.Close()
			return nil, fmt.Errorf("stats: stamping user_version: %w", err)
		}
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
	SessionID     string
	SessionName   string // human-readable name, e.g. "swift-falcon"
	Workspace     string // absolute path to the project root
	Tool          string
	CalledAt      time.Time
	DurationMs    int64
	InputBytes    int
	OutputBytes   int
	Success       bool
	ErrorMsg      string
	InputJSON     string // raw JSON args as sent to the tool (capped at 64 KiB)
	OutputText    string // full tool output (capped at 64 KiB)
	ClientName    string // MCP clientInfo.name (e.g. "claude-code")
	ClientVersion string // MCP clientInfo.version
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

// insertCallSQL inserts one tool_calls row. Shared by Record and RecordBatch.
const insertCallSQL = `INSERT INTO tool_calls
	 (session_id, session_name, workspace, tool, called_at, duration_ms, input_bytes, output_bytes, success, error_msg, input_json, output_text, client_name, client_version)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// validateCall reports the required-field error for c, or nil when storable.
func validateCall(c Call) error {
	switch {
	case c.Workspace == "":
		return fmt.Errorf("stats: workspace is required")
	case c.SessionID == "":
		return fmt.Errorf("stats: session_id is required")
	case c.Tool == "":
		return fmt.Errorf("stats: tool is required")
	}
	return nil
}

// callArgs returns the positional bind arguments for insertCallSQL.
func callArgs(c Call) []any {
	success := 1
	if !c.Success {
		success = 0
	}
	return []any{
		c.SessionID, c.SessionName, c.Workspace, c.Tool,
		c.CalledAt.UnixMilli(), c.DurationMs,
		c.InputBytes, c.OutputBytes,
		success, c.ErrorMsg,
		capString(c.InputJSON), capString(c.OutputText),
		c.ClientName, c.ClientVersion,
	}
}

// Record inserts a call. Stats are best-effort, but the caller gets the
// insert error so the daemon can log storage failures.
func (d *DB) Record(c Call) error {
	if d == nil {
		return nil
	}
	if err := validateCall(c); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.db.Exec(insertCallSQL, callArgs(c)...); err != nil {
		return fmt.Errorf("stats: insert call: %w", err)
	}
	return nil
}

// RecordBatch inserts many calls in one transaction — a single fsync and one
// write-lock acquisition for the whole batch instead of per row, which is what
// keeps the writer off SQLITE_BUSY under load. Rows that fail validation are
// skipped and counted; a SQLite error rolls the whole transaction back.
func (d *DB) RecordBatch(calls []Call) (skipped int, err error) {
	if d == nil || len(calls) == 0 {
		return 0, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("stats: begin batch: %w", err)
	}
	stmt, err := tx.Prepare(insertCallSQL)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("stats: prepare batch: %w", err)
	}
	defer stmt.Close()
	for _, c := range calls {
		if validateCall(c) != nil {
			skipped++
			continue
		}
		if _, err := stmt.Exec(callArgs(c)...); err != nil {
			_ = tx.Rollback()
			return skipped, fmt.Errorf("stats: insert batch: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return skipped, fmt.Errorf("stats: commit batch: %w", err)
	}
	return skipped, nil
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

// checkpoint truncates the WAL back into the main database file, bounding WAL
// growth between the autocheckpoint thresholds. Best-effort: a checkpoint
// blocked by a live reader is left for the next attempt.
func (d *DB) checkpoint() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
}
