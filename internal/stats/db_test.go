package stats

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecent_RoundTripsErrorAndByteFields(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "stats.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	if err := db.Record(Call{
		SessionID:   "sess-1",
		Tool:        "find_symbol",
		CalledAt:    now,
		DurationMs:  42,
		InputBytes:  128,
		OutputBytes: 4096,
		Success:     false,
		ErrorMsg:    "uri is required",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := db.Recent(10, Filter{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	c := got[0]
	if c.Tool != "find_symbol" {
		t.Errorf("Tool = %q, want find_symbol", c.Tool)
	}
	if c.Success {
		t.Errorf("Success = true, want false")
	}
	if c.ErrorMsg != "uri is required" {
		t.Errorf("ErrorMsg = %q, want %q", c.ErrorMsg, "uri is required")
	}
	if c.InputBytes != 128 {
		t.Errorf("InputBytes = %d, want 128", c.InputBytes)
	}
	if c.OutputBytes != 4096 {
		t.Errorf("OutputBytes = %d, want 4096", c.OutputBytes)
	}
	if c.DurationMs != 42 {
		t.Errorf("DurationMs = %d, want 42", c.DurationMs)
	}
}

func TestRecent_SuccessfulCallHasEmptyErrorMsg(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "stats.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Record(Call{
		SessionID:  "sess-1",
		Tool:       "list_symbols",
		CalledAt:   time.Now(),
		DurationMs: 5,
		Success:    true,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, _ := db.Recent(10, Filter{})
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].ErrorMsg != "" {
		t.Errorf("successful call ErrorMsg = %q, want empty", got[0].ErrorMsg)
	}
	if !got[0].Success {
		t.Errorf("Success = false, want true")
	}
}

func TestPerProjectDBSurvivesWorkspaceMove(t *testing.T) {
	parent := t.TempDir()
	oldWorkspace := filepath.Join(parent, "old", "plumb")
	newWorkspace := filepath.Join(parent, "new", "plumb")

	db, err := Open(DBPathFor(oldWorkspace))
	if err != nil {
		t.Fatalf("Open old workspace DB: %v", err)
	}
	if err := db.Record(Call{
		SessionID: "sess-1",
		Tool:      "read_file",
		CalledAt:  time.Now(),
		Success:   true,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	db.Close()

	if err := os.MkdirAll(filepath.Dir(newWorkspace), 0o755); err != nil {
		t.Fatalf("MkdirAll new parent: %v", err)
	}
	if err := os.Rename(oldWorkspace, newWorkspace); err != nil {
		t.Fatalf("Rename workspace: %v", err)
	}

	moved, err := OpenReadOnly(DBPathFor(newWorkspace))
	if err != nil {
		t.Fatalf("OpenReadOnly moved workspace DB: %v", err)
	}
	if moved == nil {
		t.Fatal("moved workspace DB was not found")
	}
	defer moved.Close()

	got, err := moved.Recent(10, Filter{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Recent rows = %d, want 1", len(got))
	}
	if got[0].Tool != "read_file" {
		t.Errorf("Tool = %q, want read_file", got[0].Tool)
	}
}

// TestOpen_IdempotentOnUnstampedAllColumnsDB reproduces the failure mode of
// the buggy build that defined input_json/output_text in the baseline CREATE
// TABLE statement: a database with all v3 columns but user_version still 0.
// Reopening it must succeed (migrations no-op for columns that already exist)
// and stamp user_version to the current SchemaVersion.
func TestOpen_IdempotentOnUnstampedAllColumnsDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.db")

	// Manually create a DB in the broken state: all v3 columns, user_version=0.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("manual open: %v", err)
	}
	_, err = raw.Exec(`
		CREATE TABLE tool_calls (
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
	`)
	if err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open on unstamped-all-columns DB failed: %v", err)
	}
	defer db.Close()

	v, err := db.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("CurrentSchemaVersion: %v", err)
	}
	if v != SchemaVersion {
		t.Errorf("user_version after recovery = %d, want %d", v, SchemaVersion)
	}

	// Confirm it works for real I/O.
	if err := db.Record(Call{Tool: "x", CalledAt: time.Now(), Success: true}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := db.Recent(10, Filter{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Recent rows = %d, want 1", len(got))
	}
}
