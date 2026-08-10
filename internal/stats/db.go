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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sqlitex"
	"github.com/plumbkit/plumb/internal/toolerror"
)

// episodicMemoriesDDL is the single source of truth for the episodic_memories
// table shape. It is embedded in both the baseline schema (fresh database) and
// the v7→v8 migration (existing database), so the two can never drift — a fresh
// DB and a migrated DB are byte-identical by construction. `CREATE TABLE IF NOT
// EXISTS` keeps both paths idempotent and overlap-safe.
const episodicMemoriesDDL = `CREATE TABLE IF NOT EXISTS episodic_memories (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace     TEXT    NOT NULL DEFAULT '',
    session_id    TEXT    NOT NULL DEFAULT '',
    session_name  TEXT    NOT NULL DEFAULT '',
    generated_at  INTEGER NOT NULL,
    summary       TEXT    NOT NULL DEFAULT '',
    touched_files TEXT    NOT NULL DEFAULT '',
    read_count    INTEGER NOT NULL DEFAULT 0,
    write_count   INTEGER NOT NULL DEFAULT 0
)`

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
    client_version TEXT    NOT NULL DEFAULT '',
    tokens_saved          INTEGER NOT NULL DEFAULT 0,
    savings_model_version INTEGER NOT NULL DEFAULT 0,
    capability_tokens     INTEGER NOT NULL DEFAULT 0,
    efficiency_tokens     INTEGER NOT NULL DEFAULT 0,
    purpose               TEXT    NOT NULL DEFAULT '',
    error_kind            TEXT    NOT NULL DEFAULT '',
    error_retryable       INTEGER NOT NULL DEFAULT 0,
    remediation_class     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tc_tool      ON tool_calls(tool);
CREATE INDEX IF NOT EXISTS idx_tc_called_at ON tool_calls(called_at);
CREATE INDEX IF NOT EXISTS idx_tc_session   ON tool_calls(session_id);
CREATE INDEX IF NOT EXISTS idx_tc_workspace ON tool_calls(workspace);
CREATE INDEX IF NOT EXISTS idx_tc_ws_session ON tool_calls(workspace, session_id);
CREATE INDEX IF NOT EXISTS idx_tc_tool_dur ON tool_calls(tool, duration_ms);

` + episodicMemoriesDDL + `;
CREATE INDEX IF NOT EXISTS idx_em_ws ON episodic_memories(workspace, generated_at);
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
	// v8 adds a new table, not a column — addColumn stays empty so the step always
	// runs; CREATE TABLE IF NOT EXISTS makes it idempotent and overlap-safe with
	// the baseline schema. Shares episodicMemoriesDDL with the baseline so the two
	// can never drift.
	{from: 7, to: 8, sql: episodicMemoriesDDL},
	// v9–v12 add the per-call savings columns for the tokens-saved redesign
	// (provenance + two-axis scoring). All default 0, so every existing row reads
	// as unscored (savings_model_version = 0) until a scorer stamps it.
	{from: 8, to: 9, addColumn: "tokens_saved", sql: `ALTER TABLE tool_calls ADD COLUMN tokens_saved          INTEGER NOT NULL DEFAULT 0`},
	{from: 9, to: 10, addColumn: "savings_model_version", sql: `ALTER TABLE tool_calls ADD COLUMN savings_model_version INTEGER NOT NULL DEFAULT 0`},
	{from: 10, to: 11, addColumn: "capability_tokens", sql: `ALTER TABLE tool_calls ADD COLUMN capability_tokens     INTEGER NOT NULL DEFAULT 0`},
	{from: 11, to: 12, addColumn: "efficiency_tokens", sql: `ALTER TABLE tool_calls ADD COLUMN efficiency_tokens     INTEGER NOT NULL DEFAULT 0`},
	// v13 adds the optional human-readable session purpose tag. Defaults to '',
	// so every existing row reads as "no purpose set".
	{from: 12, to: 13, addColumn: "purpose", sql: `ALTER TABLE tool_calls ADD COLUMN purpose               TEXT NOT NULL DEFAULT ''`},
	// v14–v16 add the failure-classification columns (internal/toolerror). All
	// three default to the "no structured claim" value, which is exactly what a
	// pre-v14 row is: plumb never classified it, and nothing here infers a kind
	// from the stored error_msg prose. A legacy failure reads back as
	// unclassified, never as a guess.
	{from: 13, to: 14, addColumn: "error_kind", sql: `ALTER TABLE tool_calls ADD COLUMN error_kind            TEXT NOT NULL DEFAULT ''`},
	{from: 14, to: 15, addColumn: "error_retryable", sql: `ALTER TABLE tool_calls ADD COLUMN error_retryable       INTEGER NOT NULL DEFAULT 0`},
	{from: 15, to: 16, addColumn: "remediation_class", sql: `ALTER TABLE tool_calls ADD COLUMN remediation_class     TEXT NOT NULL DEFAULT ''`},
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
//	8 — added episodic_memories table (0.9.10+)
//	9 — added tokens_saved column (tokens-saved redesign P0)
//	10 — added savings_model_version column (tokens-saved redesign P0)
//	11 — added capability_tokens column (tokens-saved redesign P0)
//	12 — added efficiency_tokens column (tokens-saved redesign P0)
//	13 — added purpose column (session purpose-tagging)
//	14 — added error_kind column (failure telemetry)
//	15 — added error_retryable column (failure telemetry)
//	16 — added remediation_class column (failure telemetry)
const SchemaVersion = 16

// Open opens (or creates) the stats database at the conventional global path.
func Open() (*DB, error) {
	path := DBPathFor()
	// SyncNormal: stats are telemetry. Under WAL this stays corruption-safe and
	// only risks losing the most recent records on a hard power cut, which is a
	// fair trade for not fsyncing on every tool call.
	db, err := sqlitex.Open(path, sqlitex.Options{Sync: sqlitex.SyncNormal, MaxOpenConns: 1})
	if err != nil {
		return nil, fmt.Errorf("stats: open %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("stats: schema: %w", err)
	}
	// Read the current schema version and apply any pending migrations. The
	// user_version stamp is a write, so only issue it when the version actually
	// moved — stamping on every Open turns a read-only open into a writer that
	// contends for the write lock.
	currentVersion, err := sqlitex.Version(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if currentVersion < SchemaVersion {
		if err := migrate(db, currentVersion, SchemaVersion); err != nil {
			db.Close()
			return nil, err
		}
		if err := sqlitex.StampVersion(db, SchemaVersion); err != nil {
			db.Close()
			return nil, err
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
	// Short timeout: a reader here is a CLI or TUI inspector that should report
	// contention rather than stall behind the daemon's writer.
	db, err := sqlitex.OpenReadOnly(path, sqlitex.ReadOnlyOptions{BusyTimeout: time.Second})
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
	currentVersion, err := sqlitex.Version(db)
	if err != nil {
		return err
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

	// Savings accounting (tokens-saved redesign). Populated at write time by the
	// scorer in the cli layer; SavingsModelVersion records which model produced
	// the figures (0 = unscored/legacy). TokensSaved is the headline total;
	// CapabilityTokens + EfficiencyTokens are the honest two-axis split.
	TokensSaved         int
	SavingsModelVersion int
	CapabilityTokens    int
	EfficiencyTokens    int

	// Purpose is the optional human-readable session purpose tag (e.g.
	// "deploy-fix"), set via session_start. Empty when unset.
	Purpose string

	// Failure classification, mirroring the `_meta` envelope the same call put on
	// the wire. Both are stamped from ONE classification made at the MCP dispatch
	// boundary, so the recorded row and the client's copy can never disagree.
	//
	// A blank ErrorKind means "plumb makes no structured claim about this
	// failure" — the same thing the envelope's absence means — and it is also
	// what every pre-v14 row reads back as. Nothing infers a kind from ErrorMsg
	// prose, so an unclassified failure stays honestly unclassified rather than
	// being folded into KindInternal.
	//
	// ErrorRetryable is derived from RemediationClass at write time (see
	// toolerror.RemediationClass.Retryable) — stored so a query can count
	// retryable failures without re-deriving, never set independently.
	ErrorKind        toolerror.Kind
	ErrorRetryable   bool
	RemediationClass toolerror.RemediationClass
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
	 (session_id, session_name, workspace, tool, called_at, duration_ms, input_bytes, output_bytes, success, error_msg, input_json, output_text, client_name, client_version, tokens_saved, savings_model_version, capability_tokens, efficiency_tokens, purpose, error_kind, error_retryable, remediation_class)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// validateCall reports the required-field error for c, or nil when storable.
// These three are the row's identity: without them it cannot be attributed to a
// workspace, a session, or a tool, and there is nothing to store.
func validateCall(c Call) error {
	switch {
	case c.Workspace == "":
		return errors.New("stats: workspace is required")
	case c.SessionID == "":
		return errors.New("stats: session_id is required")
	case c.Tool == "":
		return errors.New("stats: tool is required")
	}
	return nil
}

// normaliseCall blanks a classification that is not in toolerror's declared
// vocabulary, and reports what it dropped.
//
// error_kind and remediation_class are GROUP BY keys, so an invented label would
// quietly split a bucket in every failure report that ever reads the table — but
// the answer to that is to drop the LABEL, not the row. The classification is
// the optional part of a telemetry row; the duration, the savings, the tool and
// the client identity are not, and refusing the whole row over a bad label would
// trade a large certain loss for a small one.
//
// An undeclared KIND takes the whole classification with it, remedy and
// retryability included. A row with no kind makes no structured claim at all, so
// a row sitting in the `unclassified` bucket while still reporting a retryable
// call is not a half-truth, it is two truths: the CLI renders that bucket's
// retryability as unknown while the TUI sums the stored flag, and the two
// surfaces would then disagree about the same rows. An undeclared CLASS is
// narrower — the kind survives, because knowing WHAT went wrong is useful
// without knowing what to do — but the retryability derived from the class does
// not, since it would be a claim with nothing behind it.
//
// Nothing can trigger this today — every classified seam uses a declared
// constant — so it is a guard against a future typo, not a live path. That is
// exactly why it must not be silent: its callers log what it reports.
func normaliseCall(c Call) (Call, string) {
	badKind := c.ErrorKind != "" && !c.ErrorKind.Valid()
	badClass := c.RemediationClass != "" && !c.RemediationClass.Valid()
	if !badKind && !badClass {
		return c, ""
	}
	// Record both labels before either is cleared, so a row that is wrong twice
	// does not report only its first fault.
	var parts []string
	if badKind {
		parts = append(parts, "error_kind="+string(c.ErrorKind))
	}
	if badClass {
		parts = append(parts, "remediation_class="+string(c.RemediationClass))
	}
	if badKind {
		c.ErrorKind = ""
	}
	c.RemediationClass = ""
	c.ErrorRetryable = false
	return c, strings.Join(parts, " ")
}

// storableCall is the one gate every insert path goes through: it normalises the
// classification and then applies the required-field check, returning whatever
// label it dropped so the CALLER can decide how to report it. It does not log:
// RecordBatch runs it once per row on the single writer goroutine, and a
// per-row warning there would be the log spam the Writer's own drop accounting
// exists to avoid (see logDropped).
func storableCall(c Call) (Call, string, error) {
	c, dropped := normaliseCall(c)
	return c, dropped, validateCall(c)
}

// callArgs returns the positional bind arguments for insertCallSQL.
func callArgs(c Call) []any {
	success := 1
	if !c.Success {
		success = 0
	}
	retryable := 0
	if c.ErrorRetryable {
		retryable = 1
	}
	return []any{
		c.SessionID, c.SessionName, c.Workspace, c.Tool,
		c.CalledAt.UnixMilli(), c.DurationMs,
		c.InputBytes, c.OutputBytes,
		success, c.ErrorMsg,
		capString(c.InputJSON), capString(c.OutputText),
		c.ClientName, c.ClientVersion,
		c.TokensSaved, c.SavingsModelVersion, c.CapabilityTokens, c.EfficiencyTokens,
		c.Purpose,
		string(c.ErrorKind), retryable, string(c.RemediationClass),
	}
}

// Record inserts a call. Stats are best-effort, but the caller gets the
// insert error so the daemon can log storage failures.
func (d *DB) Record(c Call) error {
	if d == nil {
		return nil
	}
	c, dropped, err := storableCall(c)
	if err != nil {
		return err
	}
	if dropped != "" {
		slog.Warn("stats: dropped an undeclared failure classification; the row is stored without it",
			"tool", c.Tool, "dropped", dropped)
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
	// Counted across the batch and logged once, rather than per row: this runs on
	// the single writer goroutine, where a line per row is the spam the Writer's
	// own drop accounting is careful to avoid.
	demoted, example := 0, ""
	for _, c := range calls {
		c, dropped, err := storableCall(c)
		if err != nil {
			skipped++
			continue
		}
		if dropped != "" {
			demoted++
			example = dropped
		}
		if _, err := stmt.Exec(callArgs(c)...); err != nil {
			_ = tx.Rollback()
			return skipped, fmt.Errorf("stats: insert batch: %w", err)
		}
	}
	if demoted > 0 {
		slog.Warn("stats: dropped undeclared failure classifications; the rows are stored without them",
			"rows", demoted, "example", example)
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
