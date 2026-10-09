package sqlitex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// readErr runs one query against path through a read-only handle and returns
// the error SQLite reports, the way an inspector such as `plumb doctor` meets it.
func readErr(t *testing.T, path, query string) error {
	t.Helper()
	db, err := OpenReadOnly(path, ReadOnlyOptions{})
	if err != nil {
		return err
	}
	defer db.Close()
	var n int64
	return db.QueryRow(query).Scan(&n)
}

func TestIsCorruptCode(t *testing.T) {
	for code, want := range map[int]bool{
		11:  true,  // SQLITE_CORRUPT
		26:  true,  // SQLITE_NOTADB
		779: true,  // SQLITE_CORRUPT_INDEX: extended codes keep the primary in the low byte
		267: true,  // SQLITE_CORRUPT_VTAB
		5:   false, // SQLITE_BUSY
		14:  false, // SQLITE_CANTOPEN
		10:  false, // SQLITE_IOERR
		522: false, // SQLITE_IOERR_SHORT_READ
	} {
		if got := isCorruptCode(code); got != want {
			t.Errorf("isCorruptCode(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestIsCorrupt(t *testing.T) {
	dir := t.TempDir()

	// SQLITE_NOTADB: a file that is not a database at all.
	text := filepath.Join(dir, "text.db")
	if err := os.WriteFile(text, []byte("this is not a SQLite database, just text long enough to have a header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := readErr(t, text, `SELECT count(*) FROM sqlite_master`); !IsCorrupt(err) {
		t.Errorf("non-database file: IsCorrupt(%v) = false, want true", err)
	}

	// SQLITE_CORRUPT: a real database cut short, so its table points at pages
	// past the end of the file.
	cut := filepath.Join(dir, "cut.db")
	db, err := Open(cut, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE t(x BLOB)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 200) INSERT INTO t SELECT randomblob(4000) FROM n`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, sidecar := range []string{cut + "-wal", cut + "-shm"} {
		_ = os.Remove(sidecar)
	}
	if err := os.Truncate(cut, 8*4096); err != nil {
		t.Fatal(err)
	}
	if err := readErr(t, cut, `SELECT sum(length(x)) FROM t`); !IsCorrupt(err) {
		t.Errorf("truncated database: IsCorrupt(%v) = false, want true", err)
	}

	// Not corruption: nothing, a plain error, a wrapped plain error, and a path
	// SQLite cannot open. None of these may earn a destructive remedy.
	if err := os.Mkdir(filepath.Join(dir, "dir.db"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"nil":         nil,
		"plain":       errors.New("database is locked"),
		"wrapped":     fmt.Errorf("stats: read: %w", errors.New("boom")),
		"unopenable":  readErr(t, filepath.Join(dir, "dir.db"), `SELECT count(*) FROM sqlite_master`),
		"missing dir": readErr(t, filepath.Join(dir, "no", "such", "x.db"), `SELECT count(*) FROM sqlite_master`),
	} {
		if IsCorrupt(err) {
			t.Errorf("%s: IsCorrupt(%v) = true, want false", name, err)
		}
	}
}
