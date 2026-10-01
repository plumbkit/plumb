package stats

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const v19ToolCallsDDL = `CREATE TABLE tool_calls (
    id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL DEFAULT '', session_name TEXT NOT NULL DEFAULT '',
    workspace TEXT NOT NULL DEFAULT '', tool TEXT NOT NULL, called_at INTEGER NOT NULL, duration_ms INTEGER NOT NULL DEFAULT 0,
    input_bytes INTEGER NOT NULL DEFAULT 0, output_bytes INTEGER NOT NULL DEFAULT 0, success INTEGER NOT NULL DEFAULT 1,
    error_msg TEXT NOT NULL DEFAULT '', input_json TEXT NOT NULL DEFAULT '', output_text TEXT NOT NULL DEFAULT '',
    client_name TEXT NOT NULL DEFAULT '', client_version TEXT NOT NULL DEFAULT '', tokens_saved INTEGER NOT NULL DEFAULT 0,
    savings_model_version INTEGER NOT NULL DEFAULT 0, capability_tokens INTEGER NOT NULL DEFAULT 0,
    efficiency_tokens INTEGER NOT NULL DEFAULT 0, purpose TEXT NOT NULL DEFAULT '', error_kind TEXT NOT NULL DEFAULT '',
    error_retryable INTEGER NOT NULL DEFAULT 0, remediation_class TEXT NOT NULL DEFAULT '', logical_agent TEXT NOT NULL DEFAULT '')`

func TestToolCallsSchemaFreshEqualsMigratedFromV19(t *testing.T) {
	fresh := openRaw(t, "fresh.db")
	if _, err := fresh.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if err := migrate(fresh, 0); err != nil {
		t.Fatal(err)
	}
	old := openRaw(t, "v19.db")
	if _, err := old.Exec(v19ToolCallsDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO tool_calls (tool, called_at) VALUES ('edit_file', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(old, 19); err != nil {
		t.Fatal(err)
	}
	if f, m := columns(t, fresh, "tool_calls"), columns(t, old, "tool_calls"); !slices.Equal(f, m) {
		t.Fatalf("tool_calls columns differ:\n fresh    %v\n migrated %v", f, m)
	}
	for _, db := range []*sql.DB{fresh, old} {
		if !slices.Contains(indexNames(t, db, "tool_calls"), "idx_tc_call") {
			t.Fatal("idx_tc_call missing (positive control: fresh AND migrated must both have it)")
		}
	}
	var callID string
	if err := old.QueryRow(`SELECT call_id FROM tool_calls`).Scan(&callID); err != nil || callID != "" {
		t.Fatalf("legacy row call_id = %q, %v; want ''", callID, err)
	}
}

func TestCallByIDFindsTheRecordedCall(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.UnixMilli(1_790_000_000_000)
	if err := db.Record(Call{Workspace: "/w", SessionID: "s", Tool: "edit_file", CalledAt: at, DurationMs: 7, Success: true, CallID: "01CALL"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.CallByID("01CALL")
	if err != nil || !ok || got.Tool != "edit_file" || got.DurationMs != 7 || !got.CalledAt.Equal(at) {
		t.Fatalf("CallByID = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := db.CallByID("nope"); ok {
		t.Fatal("unknown id reported found")
	}
}

func openRaw(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func columns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name, type, "notnull", COALESCE(dflt_value,'') FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n, ty, d string
		var nn int
		if err := rows.Scan(&n, &ty, &nn, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, n+" "+ty+" "+d)
	}
	return out
}

func indexNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_index_list(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}
