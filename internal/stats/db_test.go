package stats

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecent_RoundTripsErrorAndByteFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	if err := db.Record(Call{
		SessionID:   "sess-1",
		Workspace:   "/w1",
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
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Record(Call{
		SessionID:  "sess-1",
		Workspace:  "/w1",
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

func TestRenameSessionBackfillsRows(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	if err := db.Record(Call{SessionID: "sess-1", Workspace: "/w1", SessionName: "OLD-NAME", Tool: "read_file", CalledAt: now, Success: true}); err != nil {
		t.Fatalf("Record first: %v", err)
	}
	if err := db.Record(Call{SessionID: "sess-2", Workspace: "/w1", SessionName: "OTHER", Tool: "read_file", CalledAt: now.Add(time.Millisecond), Success: true}); err != nil {
		t.Fatalf("Record second: %v", err)
	}

	if err := db.RenameSession("sess-1", "NEW-NAME"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	got, err := db.Recent(10, Filter{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	bySession := make(map[string]string)
	for _, c := range got {
		bySession[c.SessionID] = c.SessionName
	}
	if bySession["sess-1"] != "NEW-NAME" {
		t.Fatalf("sess-1 name = %q, want NEW-NAME", bySession["sess-1"])
	}
	if bySession["sess-2"] != "OTHER" {
		t.Fatalf("sess-2 name = %q, want OTHER", bySession["sess-2"])
	}
}

func TestActivityAtBucketsRecentCalls(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.UnixMilli(1_000_000)
	calls := []Call{
		{SessionID: "sess-1", Workspace: "/w1", Tool: "read_file", CalledAt: now.Add(-59 * time.Second), Success: true},
		{SessionID: "sess-1", Workspace: "/w1", Tool: "edit_file", CalledAt: now.Add(-30 * time.Second), Success: true},
		{SessionID: "sess-1", Workspace: "/w1", Tool: "git", CalledAt: now.Add(-1 * time.Second), Success: true},
		{SessionID: "sess-1", Workspace: "/w1", Tool: "old", CalledAt: now.Add(-2 * time.Minute), Success: true},
		{SessionID: "sess-2", Workspace: "/w1", Tool: "other", CalledAt: now.Add(-1 * time.Second), Success: true},
	}
	for _, c := range calls {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record %s: %v", c.Tool, err)
		}
	}

	got, err := db.ActivityAt(now, time.Minute, 6, Filter{SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("ActivityAt: %v", err)
	}
	if got.Calls != 3 {
		t.Fatalf("Calls = %d, want 3", got.Calls)
	}
	wantBuckets := []int64{1, 0, 0, 1, 0, 1}
	for i, want := range wantBuckets {
		if got.Buckets[i] != want {
			t.Fatalf("bucket %d = %d, want %d (all buckets %#v)", i, got.Buckets[i], want, got.Buckets)
		}
	}
}

func TestPerProjectDBSurvivesWorkspaceMove(t *testing.T) {
	// This test is now deprecated/removed because stats are global.
}

func TestOpen_IdempotentOnUnstampedAllColumnsDB(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	path := DBPathFor()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("manual open: %v", err)
	}
	_, err = raw.Exec(`
		CREATE TABLE tool_calls (
		    id           INTEGER PRIMARY KEY AUTOINCREMENT,
		    session_id   TEXT    NOT NULL DEFAULT '',
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

	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if v != SchemaVersion {
		t.Errorf("user_version = %d, want %d", v, SchemaVersion)
	}
}

func TestOpenReadOnly_OldSchemaReturnsUpgradeError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	path := DBPathFor()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	raw, _ := sql.Open("sqlite", path)
	_, _ = raw.Exec(`CREATE TABLE tool_calls (id INTEGER PRIMARY KEY, tool TEXT, called_at INTEGER);`)
	_, _ = raw.Exec(`PRAGMA user_version = 1;`)
	raw.Close()

	db, err := OpenReadOnly()
	if db != nil {
		db.Close()
	}
	if !errors.Is(err, ErrReadOnlySchemaUpgradeRequired) {
		t.Fatalf("OpenReadOnly old schema error = %v, want ErrReadOnlySchemaUpgradeRequired", err)
	}
}

func TestOpenReadOnly_UnstampedAllColumnsDBAllowed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	path := DBPathFor()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("manual open: %v", err)
	}
	_, err = raw.Exec(`
		CREATE TABLE tool_calls (
		    id           INTEGER PRIMARY KEY AUTOINCREMENT,
		    session_id   TEXT    NOT NULL DEFAULT '',
		    tool         TEXT    NOT NULL,
		    workspace    TEXT    NOT NULL DEFAULT '',
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
	if _, err := raw.Exec(
		`INSERT INTO tool_calls
		 (session_id, tool, workspace, called_at, duration_ms, input_bytes, output_bytes, success, error_msg, input_json, output_text)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"sess-1", "read_file", "/w1", time.Now().UnixMilli(), 1, 2, 3, 1, "", "{}", "ok",
	); err != nil {
		t.Fatalf("insert row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	db, err := OpenReadOnly()
	if err != nil {
		t.Fatalf("OpenReadOnly unstamped all-columns DB: %v", err)
	}
	defer db.Close()
	got, err := db.Recent(10, Filter{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 || got[0].InputJSON != "{}" || got[0].OutputText != "ok" {
		t.Fatalf("Recent = %#v, want row with detail columns", got)
	}
}
