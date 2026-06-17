package topology

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDB_CreatesSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "topology.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Check file exists on disk.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db file not created: %v", err)
	}

	tables := []string{
		"topology_meta",
		"topology_files",
		"topology_nodes",
		"topology_edges",
	}
	for _, table := range tables {
		var name string
		row := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table)
		if err := row.Scan(&name); err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}
}

func TestOpenDB_FreshStampsVersionAndSpanColumns(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion)
	}
	for _, col := range []string{"has_bytes", "start_byte", "end_byte", "start_col", "end_col", "doc_start_byte", "doc_end_byte"} {
		if !columnExists(t, db, "topology_nodes", col) {
			t.Errorf("topology_nodes is missing the %q column on a fresh database", col)
		}
	}
}

func TestOpenDB_OldVersionRecreatesSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "topology.db")

	// Simulate a pre-versioned (user_version 0) database carrying the OLD
	// topology_nodes column set, with a row in it.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE topology_nodes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		file_id INTEGER NOT NULL DEFAULT 0,
		kind TEXT NOT NULL,
		name TEXT NOT NULL DEFAULT '',
		start_line INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO topology_nodes(kind, name) VALUES ('function', 'old')`); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	raw.Close()

	// Re-open through openDB: the version gate must drop and recreate the schema
	// with the new span columns, and stamp the current version.
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB after old schema: %v", err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion)
	}
	for _, col := range []string{"has_bytes", "start_byte", "doc_end_byte"} {
		if !columnExists(t, db, "topology_nodes", col) {
			t.Errorf("topology_nodes is missing the %q column after recreate", col)
		}
	}
	// The recreated table is empty (the stale row was dropped), and an INSERT
	// naming the new columns succeeds — the bug the gate exists to prevent.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM topology_nodes`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("recreated topology_nodes row count = %d, want 0", count)
	}
	fileID := insertTestFile(t, db, "m.go")
	if _, err := db.Exec(`INSERT INTO topology_nodes(file_id, kind, name, has_bytes, start_byte, end_byte) VALUES (?, 'function', 'new', 1, 3, 9)`, fileID); err != nil {
		t.Errorf("insert naming new columns failed after recreate: %v", err)
	}
}

func columnExists(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == col {
			return true
		}
	}
	return false
}

func TestOpenDB_FTS5Table(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	var name string
	row := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='topology_fts'`)
	if err := row.Scan(&name); err != nil {
		t.Errorf("FTS5 virtual table missing: %v", err)
	}
}

func TestOpenDB_WALMode(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "wal.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("expected WAL mode, got %q", mode)
	}
}

func TestOpenDB_Idempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "topo.db")
	// Opening twice should succeed without schema conflicts.
	for i := range 2 {
		db, err := openDB(dbPath)
		if err != nil {
			t.Fatalf("openDB attempt %d: %v", i+1, err)
		}
		db.Close()
	}
}

func TestDBPath(t *testing.T) {
	got := DBPath("/project")
	want := "/project/.plumb/topology.db"
	if got != want {
		t.Errorf("DBPath = %q, want %q", got, want)
	}
}

// insertTestFile is a helper that inserts a file row and returns its ID.
func insertTestFile(t *testing.T, db *sql.DB, path string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO topology_files(path, mtime_ns, content_hash, indexed_at, error_msg) VALUES (?,?,?,?,?)`,
		path, 0, "abc", 0, "")
	if err != nil {
		t.Fatalf("insert file %q: %v", path, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestInsertNodes_SpanRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "m.go")
	want := []Node{
		{
			Kind: KindFunction, Name: "WithSpan", StartLine: 2, EndLine: 4,
			HasBytes: true, StartByte: 10, EndByte: 42, StartCol: 0, EndCol: 1,
			DocStartByte: 3, DocEndByte: 9, Language: "go",
		},
		{Kind: KindConstant, Name: "NoSpan", StartLine: 6, EndLine: 6, Language: "go"},
	}
	persistNodes(t, db, fileID, want)

	s := &Store{workspace: dir, db: db}
	got, err := s.SymbolsInFile(context.Background(), "m.go")
	if err != nil {
		t.Fatalf("SymbolsInFile: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d nodes, want 2", len(got))
	}
	assertSpanRoundTrip(t, got[0], want[0])

	none := got[1]
	if none.HasBytes {
		t.Error("NoSpan should round-trip HasBytes=false (the absent-span sentinel)")
	}
	if none.HasDocSpan() {
		t.Error("NoSpan should have no doc span")
	}
}

func persistNodes(t *testing.T, db *sql.DB, fileID int64, nodes []Node) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := insertNodes(tx, fileID, "m.go", nodes); err != nil {
		t.Fatalf("insertNodes: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func assertSpanRoundTrip(t *testing.T, got, want Node) {
	t.Helper()
	if !got.HasBytes {
		t.Error("node should round-trip HasBytes=true")
	}
	if got.StartByte != want.StartByte || got.EndByte != want.EndByte ||
		got.StartCol != want.StartCol || got.EndCol != want.EndCol {
		t.Errorf("decl span = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
			got.StartByte, got.EndByte, got.StartCol, got.EndCol,
			want.StartByte, want.EndByte, want.StartCol, want.EndCol)
	}
	if got.DocStartByte != want.DocStartByte || got.DocEndByte != want.DocEndByte || !got.HasDocSpan() {
		t.Errorf("doc span = (%d,%d), want (%d,%d)",
			got.DocStartByte, got.DocEndByte, want.DocStartByte, want.DocEndByte)
	}
}
