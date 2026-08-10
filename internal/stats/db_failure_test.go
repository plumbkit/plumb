package stats

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// v13ColumnsDDL is the tool_calls table exactly as schema version 13 left it —
// every column up to and including purpose, with the closing paren left off so
// the v16 shape can extend it. Seeding the real shape (rather than a two-column
// stub) is what makes the upgrade tests exercise the same ALTER path a user's
// database will take.
const v13ColumnsDDL = `CREATE TABLE tool_calls (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL DEFAULT '',
	session_name TEXT NOT NULL DEFAULT '',
	workspace TEXT NOT NULL DEFAULT '',
	tool TEXT NOT NULL,
	called_at INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	input_bytes INTEGER NOT NULL DEFAULT 0,
	output_bytes INTEGER NOT NULL DEFAULT 0,
	success INTEGER NOT NULL DEFAULT 1,
	error_msg TEXT NOT NULL DEFAULT '',
	input_json TEXT NOT NULL DEFAULT '',
	output_text TEXT NOT NULL DEFAULT '',
	client_name TEXT NOT NULL DEFAULT '',
	client_version TEXT NOT NULL DEFAULT '',
	tokens_saved INTEGER NOT NULL DEFAULT 0,
	savings_model_version INTEGER NOT NULL DEFAULT 0,
	capability_tokens INTEGER NOT NULL DEFAULT 0,
	efficiency_tokens INTEGER NOT NULL DEFAULT 0,
	purpose TEXT NOT NULL DEFAULT ''`

const v13Shape = v13ColumnsDDL + `
)`

// v16Shape is v13 plus the three failure-classification columns — the shape a
// build whose baseline schema already declared them would leave behind.
const v16Shape = v13ColumnsDDL + `,
	error_kind TEXT NOT NULL DEFAULT '',
	error_retryable INTEGER NOT NULL DEFAULT 0,
	remediation_class TEXT NOT NULL DEFAULT ''
)`

// seedStatsDB writes a tool_calls table of the given DDL at the conventional
// global path, stamps user_version, and inserts one failed legacy row whose
// error_msg is prose that a naive backfill would be tempted to parse.
func seedStatsDB(t *testing.T, ddl string, version int) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	path := DBPathFor()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if _, err := seed.Exec(ddl); err != nil {
		t.Fatalf("seed tool_calls: %v", err)
	}
	if _, err := seed.Exec(
		`INSERT INTO tool_calls (session_id, workspace, tool, called_at, success, error_msg)
		 VALUES ('legacy', '/w', 'edit_file', 1, 0, ?)`,
		`"a.go" has uncommitted changes; review and commit first, or pass dirty_ok: true`,
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if _, err := seed.Exec("PRAGMA user_version = " + strconv.Itoa(version)); err != nil {
		t.Fatalf("stamp user_version: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
}

// assertColumnsExist fails unless tool_calls carries every named column. It
// probes with a zero-row SELECT rather than hasColumn: SQLite rejects an unknown
// column at prepare time, so the query itself is the assertion.
func assertColumnsExist(t *testing.T, db *sql.DB, cols string) {
	t.Helper()
	rows, err := db.Query(`SELECT ` + cols + ` FROM tool_calls LIMIT 0`)
	if err != nil {
		t.Fatalf("tool_calls is missing one of (%s): %v", cols, err)
	}
	rows.Close()
}

const failureColumns = "error_kind, error_retryable, remediation_class"

// assertLegacyRowUnclassified is the load-bearing assertion of the whole
// migration: a row recorded before plumb could classify anything must read back
// as "no structured claim", NOT as a kind inferred from its error_msg prose. The
// seeded message is a verbatim dirty-file refusal, so a backfill that pattern-
// matched English would stamp it dirty_file and this would catch it.
func assertLegacyRowUnclassified(t *testing.T, db *sql.DB) {
	t.Helper()
	var kind, class string
	var retryable int
	err := db.QueryRow(
		`SELECT error_kind, error_retryable, remediation_class FROM tool_calls WHERE session_id='legacy'`,
	).Scan(&kind, &retryable, &class)
	if err != nil {
		t.Fatalf("scan legacy classification (column missing?): %v", err)
	}
	if kind != "" || class != "" || retryable != 0 {
		t.Fatalf("legacy row classified as (%q, %d, %q), want blank/0/blank — a pre-v14 failure "+
			"carries no structured claim and must never be guessed from its error_msg", kind, retryable, class)
	}
}

// TestMigrateFrom13AddsFailureColumns exercises the real Open() upgrade over a
// database stamped at exactly the previous schema version, so the v13→v16 steps
// run in isolation.
func TestMigrateFrom13AddsFailureColumns(t *testing.T) {
	seedStatsDB(t, v13Shape, 13)

	db, err := Open()
	if err != nil {
		t.Fatalf("Open (upgrade): %v", err)
	}
	defer db.Close()

	var v int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if v != SchemaVersion {
		t.Fatalf("user_version after upgrade = %d, want %d", v, SchemaVersion)
	}
	assertColumnsExist(t, db.db, failureColumns)
	assertLegacyRowUnclassified(t, db.db)
}

// TestMigrateFromOlderVersionReachesCurrent proves the steps compose: a v8-era
// database walks the whole ladder in one Open, not just the newest rung.
func TestMigrateFromOlderVersionReachesCurrent(t *testing.T) {
	seedStatsDB(t, `CREATE TABLE tool_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL DEFAULT '',
		session_name TEXT NOT NULL DEFAULT '',
		workspace TEXT NOT NULL DEFAULT '',
		tool TEXT NOT NULL,
		called_at INTEGER NOT NULL,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		input_bytes INTEGER NOT NULL DEFAULT 0,
		output_bytes INTEGER NOT NULL DEFAULT 0,
		success INTEGER NOT NULL DEFAULT 1,
		error_msg TEXT NOT NULL DEFAULT '',
		input_json TEXT NOT NULL DEFAULT '',
		output_text TEXT NOT NULL DEFAULT '',
		client_name TEXT NOT NULL DEFAULT '',
		client_version TEXT NOT NULL DEFAULT ''
	)`, 7)

	db, err := Open()
	if err != nil {
		t.Fatalf("Open (upgrade): %v", err)
	}
	defer db.Close()

	assertColumnsExist(t, db.db, failureColumns)
	assertColumnsExist(t, db.db, "tokens_saved, purpose")
	assertLegacyRowUnclassified(t, db.db)
}

// TestMigrateFailureColumnsIsIdempotent covers the recovery case the migration
// machinery exists for: a database whose columns are already present but whose
// user_version still says 13, as a build that declared them in the baseline
// schema would leave it. Every ADD COLUMN step must be skipped by the hasColumn
// guard — without it SQLite fails the upgrade with "duplicate column name" and
// the database is stuck below the current version forever.
func TestMigrateFailureColumnsIsIdempotent(t *testing.T) {
	seedStatsDB(t, v16Shape, 13)

	db, err := Open()
	if err != nil {
		t.Fatalf("Open over already-present columns stamped v13: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if v != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", v, SchemaVersion)
	}
	assertColumnsExist(t, db.db, failureColumns)
	assertLegacyRowUnclassified(t, db.db)
}

// TestFailureClassificationRoundTrips proves the four-way plumbing (Call,
// insertCallSQL, callArgs, validateCall) moves in step: what is set on the
// struct is what comes back out of the row, and an unclassified call stores
// blanks rather than a placeholder.
func TestFailureClassificationRoundTrips(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	classified := Call{
		SessionID: "s1", Workspace: "/w", Tool: "edit_file", CalledAt: time.Now(),
		ErrorKind:        toolerror.KindDirtyFile,
		RemediationClass: toolerror.ClassPassDirtyOk,
		ErrorRetryable:   toolerror.ClassPassDirtyOk.Retryable(),
	}
	if err := db.Record(classified); err != nil {
		t.Fatalf("Record classified: %v", err)
	}
	if err := db.Record(Call{SessionID: "s2", Workspace: "/w", Tool: "read_file", CalledAt: time.Now(), Success: true}); err != nil {
		t.Fatalf("Record success: %v", err)
	}

	var kind, class string
	var retryable int
	if err := db.db.QueryRow(
		`SELECT error_kind, error_retryable, remediation_class FROM tool_calls WHERE session_id='s1'`,
	).Scan(&kind, &retryable, &class); err != nil {
		t.Fatalf("scan s1: %v", err)
	}
	if kind != string(toolerror.KindDirtyFile) || class != string(toolerror.ClassPassDirtyOk) || retryable != 1 {
		t.Fatalf("s1 = (%q, %d, %q), want (dirty_file, 1, pass_dirty_ok)", kind, retryable, class)
	}

	if err := db.db.QueryRow(
		`SELECT error_kind, error_retryable, remediation_class FROM tool_calls WHERE session_id='s2'`,
	).Scan(&kind, &retryable, &class); err != nil {
		t.Fatalf("scan s2: %v", err)
	}
	if kind != "" || class != "" || retryable != 0 {
		t.Fatalf("successful call = (%q, %d, %q), want all blank", kind, retryable, class)
	}
}

// TestUndeclaredClassificationIsBlankedNotDropped pins the trade at the write
// path: an invented label must not reach the GROUP BY keys, but the answer is to
// drop the LABEL, not the row. The classification is the optional part of a
// telemetry row; the duration, savings, tool and client identity are not.
func TestUndeclaredClassificationIsBlankedNotDropped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	bad := Call{
		SessionID: "s1", Workspace: "/w", Tool: "edit_file", CalledAt: time.Now(),
		DurationMs: 42, TokensSaved: 99,
		ErrorKind: "made_up", RemediationClass: "do_a_barrel_roll", ErrorRetryable: true,
	}
	if err := db.Record(bad); err != nil {
		t.Fatalf("Record with an undeclared classification returned %v; the row must still be stored", err)
	}

	var kind, class string
	var retryable int
	var durationMs, tokens int64
	if err := db.db.QueryRow(
		`SELECT error_kind, error_retryable, remediation_class, duration_ms, tokens_saved FROM tool_calls WHERE session_id='s1'`,
	).Scan(&kind, &retryable, &class, &durationMs, &tokens); err != nil {
		t.Fatalf("the row was not stored at all: %v", err)
	}
	if kind != "" || class != "" || retryable != 0 {
		t.Errorf("undeclared classification survived as (%q, %d, %q), want all blank", kind, retryable, class)
	}
	if durationMs != 42 || tokens != 99 {
		t.Errorf("the rest of the row was lost: duration=%d tokens=%d, want 42/99", durationMs, tokens)
	}
}

// TestBatchKeepsRowsWithUndeclaredClassification is the same guarantee on the
// path telemetry actually takes — the Writer's batched transaction — where a
// rejection would have been counted as `skipped` and thrown away.
func TestBatchKeepsRowsWithUndeclaredClassification(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	base := Call{Workspace: "/w", Tool: "edit_file", CalledAt: time.Now()}
	good, bad := base, base
	good.SessionID, good.ErrorKind, good.RemediationClass = "good", toolerror.KindDirtyFile, toolerror.ClassPassDirtyOk
	bad.SessionID, bad.ErrorKind = "bad", "made_up"

	skipped, err := db.RecordBatch([]Call{good, bad})
	if err != nil {
		t.Fatalf("RecordBatch: %v", err)
	}
	if skipped != 0 {
		t.Errorf("RecordBatch skipped %d rows; an undeclared label must not cost the row", skipped)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM tool_calls`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("stored %d rows, want 2", n)
	}
}

// TestDeclaredClassificationsAllSurvive is the other half: every value the
// closed vocabularies declare must pass the normaliser untouched, so the guard
// cannot quietly start blanking real classifications.
func TestDeclaredClassificationsAllSurvive(t *testing.T) {
	base := Call{SessionID: "s", Workspace: "/w", Tool: "edit_file"}
	for _, k := range toolerror.AllKinds() {
		in := base
		in.ErrorKind = k
		if got, dropped := normaliseCall(in); dropped != "" || got.ErrorKind != k {
			t.Errorf("declared kind %q was blanked (dropped=%q)", k, dropped)
		}
	}
	for _, c := range toolerror.AllRemediationClasses() {
		in := base
		in.RemediationClass = c
		in.ErrorRetryable = c.Retryable()
		got, dropped := normaliseCall(in)
		if dropped != "" || got.RemediationClass != c || got.ErrorRetryable != c.Retryable() {
			t.Errorf("declared class %q was blanked (dropped=%q)", c, dropped)
		}
	}
}
