package topology

import (
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
	for i := 0; i < 2; i++ {
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
