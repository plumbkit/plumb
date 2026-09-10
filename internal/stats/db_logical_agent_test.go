package stats

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMigrateAddsLogicalAgentColumn: a v18 database gains the column through
// the real Open() upgrade path, and every legacy row reads back "" — no claim
// about which agent, never a guess.
func TestMigrateAddsLogicalAgentColumn(t *testing.T) {
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
	// The full v18 tool_calls shape: every column except logical_agent.
	if _, err := seed.Exec(`CREATE TABLE tool_calls (
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
		purpose TEXT NOT NULL DEFAULT '',
		error_kind TEXT NOT NULL DEFAULT '',
		error_retryable INTEGER NOT NULL DEFAULT 0,
		remediation_class TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("seed tool_calls: %v", err)
	}
	if _, err := seed.Exec(`INSERT INTO tool_calls (session_id, session_name, workspace, tool, called_at, input_json) VALUES ('legacy', 'old-owl', '/w', 'edit_file', 1, '{"file_path":"/w/a.go"}')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if _, err := seed.Exec(`PRAGMA user_version = 18`); err != nil {
		t.Fatalf("stamp user_version: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

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
	got, err := db.RecentWritesByWorkspace("/w", []string{"edit_file"}, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("RecentWritesByWorkspace on the upgraded DB: %d rows, %v", len(got), err)
	}
	if got[0].LogicalAgent != "" {
		t.Fatalf("legacy row LogicalAgent = %q, want \"\"", got[0].LogicalAgent)
	}
	if got[0].SessionName != "old-owl" {
		t.Fatalf("legacy row lost its session name: %q", got[0].SessionName)
	}
}

// TestLogicalAgentRoundTrips: the recorded id comes back through both recent
// queries, and a call that declared none reads back "".
func TestLogicalAgentRoundTrips(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	calls := []Call{
		{SessionID: "s1", SessionName: "me-fox", Workspace: "/w", Tool: "write_file", CalledAt: now, Success: true, InputJSON: `{"file_path":"/w/a.go"}`, LogicalAgent: "conv-1/agent-7"},
		{SessionID: "s1", SessionName: "me-fox", Workspace: "/w", Tool: "write_file", CalledAt: now.Add(-time.Second), Success: true, InputJSON: `{"file_path":"/w/b.go"}`, LogicalAgent: "conv-1"},
		{SessionID: "s1", SessionName: "me-fox", Workspace: "/w", Tool: "write_file", CalledAt: now.Add(-2 * time.Second), Success: true, InputJSON: `{"file_path":"/w/c.go"}`},
	}
	for _, c := range calls {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	want := map[string]string{`{"file_path":"/w/a.go"}`: "conv-1/agent-7", `{"file_path":"/w/b.go"}`: "conv-1", `{"file_path":"/w/c.go"}`: ""}

	writes, err := db.RecentWritesByWorkspace("/w", []string{"write_file"}, 10)
	if err != nil || len(writes) != 3 {
		t.Fatalf("RecentWritesByWorkspace: %d rows, %v", len(writes), err)
	}
	for _, w := range writes {
		if w.LogicalAgent != want[w.InputJSON] {
			t.Errorf("write %s LogicalAgent = %q, want %q", w.InputJSON, w.LogicalAgent, want[w.InputJSON])
		}
	}
	recent, err := db.Recent(10, Filter{})
	if err != nil || len(recent) != 3 {
		t.Fatalf("Recent: %d rows, %v", len(recent), err)
	}
	for _, c := range recent {
		if c.LogicalAgent != want[c.InputJSON] {
			t.Errorf("recent %s LogicalAgent = %q, want %q", c.InputJSON, c.LogicalAgent, want[c.InputJSON])
		}
	}
}

func TestAgentLabel(t *testing.T) {
	cases := []struct{ name, ext, agent, want string }{
		{"me-fox", "", "", "me-fox"},
		{"me-fox", "conv-1", "", "me-fox"},
		{"me-fox", "conv-1", "conv-1", "me-fox"},
		{"me-fox", "conv-1", "conv-1/a1b2c3d4e5f6", "me-fox/agent-a1b2c3d4"},
		{"me-fox", "", "conv-1/x", "me-fox/agent-x"},
		{"me-fox", "conv-1", "agent-alpha", "me-fox/agent-al"},
		{"me-fox", "", "conv-1", "me-fox/conv-1"},
	}
	for _, tc := range cases {
		if got := AgentLabel(tc.name, tc.ext, tc.agent); got != tc.want {
			t.Errorf("AgentLabel(%q, %q, %q) = %q, want %q", tc.name, tc.ext, tc.agent, got, tc.want)
		}
	}
}
