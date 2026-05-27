package stats

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestRecordRequiresWorkspaceSessionAndTool(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tests := []struct {
		name string
		call Call
	}{
		{name: "workspace", call: Call{SessionID: "sess-1", Tool: "read_file", CalledAt: time.Now(), Success: true}},
		{name: "session", call: Call{Workspace: "/w1", Tool: "read_file", CalledAt: time.Now(), Success: true}},
		{name: "tool", call: Call{SessionID: "sess-1", Workspace: "/w1", CalledAt: time.Now(), Success: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := db.Record(tt.call); err == nil {
				t.Fatalf("Record() error = nil, want required-field error")
			}
		})
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

	got, err := db.ActivityAt(now, time.Minute, 6, Filter{Workspace: "/w1", SessionID: "sess-1"})
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

func TestSummaryScopesBySince(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	calls := []Call{
		{SessionID: "sess-1", Workspace: "/w1", Tool: "read_file", CalledAt: now.Add(-2 * time.Hour), DurationMs: 100, OutputBytes: 400, Success: true},
		{SessionID: "sess-1", Workspace: "/w1", Tool: "edit_file", CalledAt: now.Add(-time.Minute), DurationMs: 200, OutputBytes: 800, Success: true},
	}
	for _, c := range calls {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record %s: %v", c.Tool, err)
		}
	}

	got, err := db.Summary(Filter{Since: now.Add(-10 * time.Minute)})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(got) != 1 || got[0].Tool != "edit_file" {
		t.Fatalf("Summary since = %#v, want only edit_file", got)
	}
}

func TestSlowest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	calls := []Call{
		{SessionID: "sess-1", Workspace: "/w1", Tool: "read_file", CalledAt: now, DurationMs: 5, Success: true},
		{SessionID: "sess-1", Workspace: "/w1", Tool: "edit_file", CalledAt: now, DurationMs: 300, Success: true},
		{SessionID: "sess-1", Workspace: "/w1", Tool: "git", CalledAt: now, DurationMs: 150, Success: true},
		{SessionID: "sess-2", Workspace: "/w1", Tool: "edit_file", CalledAt: now, DurationMs: 999, Success: true},
	}
	for _, c := range calls {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record %s: %v", c.Tool, err)
		}
	}

	got, err := db.Slowest(2, Filter{SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Slowest: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Slowest returned %d rows, want 2 (limit)", len(got))
	}
	if got[0].Tool != "edit_file" || got[0].DurationMs != 300 {
		t.Fatalf("slowest[0] = %s/%dms, want edit_file/300ms", got[0].Tool, got[0].DurationMs)
	}
	if got[1].Tool != "git" || got[1].DurationMs != 150 {
		t.Fatalf("slowest[1] = %s/%dms, want git/150ms", got[1].Tool, got[1].DurationMs)
	}
	// The 999ms sess-2 call must be excluded by the SessionID filter.
	for _, c := range got {
		if c.SessionID != "sess-1" {
			t.Fatalf("Slowest leaked session %s into the sess-1 result", c.SessionID)
		}
	}
}

func TestFilterScopesSessionInsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	calls := []Call{
		{SessionID: "sess-1", Workspace: "/w1", Tool: "read_file", CalledAt: now, Success: true},
		{SessionID: "sess-1", Workspace: "/w2", Tool: "edit_file", CalledAt: now.Add(time.Millisecond), Success: true},
		{SessionID: "sess-2", Workspace: "/w1", Tool: "git", CalledAt: now.Add(2 * time.Millisecond), Success: true},
	}
	for _, c := range calls {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record %s/%s/%s: %v", c.Workspace, c.SessionID, c.Tool, err)
		}
	}

	filter := Filter{Workspace: "/w1", SessionID: "sess-1"}
	if got := db.TotalCalls(filter); got != 1 {
		t.Fatalf("TotalCalls = %d, want 1", got)
	}
	if got := db.TotalSessions(Filter{}); got != 2 {
		t.Fatalf("TotalSessions = %d, want 2", got)
	}
	if got := db.TotalSessions(Filter{Workspace: "/w1"}); got != 2 {
		t.Fatalf("TotalSessions scoped to /w1 = %d, want 2", got)
	}
	if got := db.TotalSessions(filter); got != 1 {
		t.Fatalf("TotalSessions scoped to /w1 sess-1 = %d, want 1", got)
	}
	recent, err := db.Recent(10, filter)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 1 || recent[0].Workspace != "/w1" || recent[0].SessionID != "sess-1" || recent[0].Tool != "read_file" {
		t.Fatalf("Recent = %#v, want only /w1 sess-1 read_file", recent)
	}
}

func TestCallDetailScopesByWorkspaceAndSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	calledAt := time.Now()
	calls := []Call{
		{SessionID: "sess-1", Workspace: "/w1", Tool: "read_file", CalledAt: calledAt, Success: true, InputJSON: `{"path":"/w1"}`, OutputText: "w1"},
		{SessionID: "sess-1", Workspace: "/w2", Tool: "read_file", CalledAt: calledAt, Success: true, InputJSON: `{"path":"/w2"}`, OutputText: "w2"},
	}
	for _, c := range calls {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record %s: %v", c.Workspace, err)
		}
	}

	input, output := db.CallDetail("/w2", "sess-1", calledAt)
	if input != `{"path":"/w2"}` || output != "w2" {
		t.Fatalf("CallDetail = (%q, %q), want w2 detail", input, output)
	}
}

func TestTotalTokensSavedSinceScopesByTime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	calls := []Call{
		{SessionID: "sess-1", Workspace: "/w1", Tool: "find_symbol", CalledAt: now.Add(-time.Hour), Success: true},
		{SessionID: "sess-1", Workspace: "/w1", Tool: "find_symbol", CalledAt: now, Success: true},
		{SessionID: "sess-1", Workspace: "/w1", Tool: "search_in_files", CalledAt: now, Success: true},
	}
	for _, c := range calls {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record %s: %v", c.Tool, err)
		}
	}

	// With empty client_name (legacy rows), the conservative unknown profile applies:
	// find_symbol=300, search_in_files=50 → 350.
	if got := db.TotalTokensSavedSince(now.Add(-time.Minute), Filter{}); got != 350 {
		t.Fatalf("TotalTokensSavedSince = %d, want 350", got)
	}
}

func TestOpenCreatesCurrentGlobalSchema(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

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
	for _, col := range []string{"session_id", "session_name", "workspace", "input_json", "output_text", "client_name", "client_version"} {
		has, err := hasColumn(db.db, "tool_calls", col)
		if err != nil {
			t.Fatalf("hasColumn(%s): %v", col, err)
		}
		if !has {
			t.Fatalf("tool_calls missing column %s", col)
		}
	}
}

func TestOpenReadOnlyCurrentSchemaAllowed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Record(Call{
		SessionID:  "sess-1",
		Workspace:  "/w1",
		Tool:       "read_file",
		CalledAt:   time.Now(),
		Success:    true,
		InputJSON:  "{}",
		OutputText: "ok",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	db.Close()

	ro, err := OpenReadOnly()
	if err != nil {
		t.Fatalf("OpenReadOnly current schema: %v", err)
	}
	defer ro.Close()
	got, err := ro.Recent(10, Filter{Workspace: "/w1"})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 || got[0].InputJSON != "{}" || got[0].OutputText != "ok" {
		t.Fatalf("Recent = %#v, want row with detail columns", got)
	}
}

func TestOpenReadOnlyOldSchemaTellsUserToDeleteDB(t *testing.T) {
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
	if _, err := raw.Exec(`CREATE TABLE tool_calls (id INTEGER PRIMARY KEY, tool TEXT, called_at INTEGER)`); err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	db, err := OpenReadOnly()
	if db != nil {
		db.Close()
	}
	if !errors.Is(err, ErrReadOnlySchemaUpgradeRequired) {
		t.Fatalf("OpenReadOnly error = %v, want ErrReadOnlySchemaUpgradeRequired", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("OpenReadOnly error = %q, want full DB path %q", err, path)
	}
	if !strings.Contains(err.Error(), "delete") || !strings.Contains(err.Error(), "fresh global stats database") {
		t.Fatalf("OpenReadOnly error = %q, want delete instruction", err)
	}
}
