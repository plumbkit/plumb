package sqlitex_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

func mustOpen(t *testing.T, opts sqlitex.Options) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "test.db")
	db, err := sqlitex.Open(path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func queryString(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(q).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

func queryInt(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// TestOpen_AppliesEveryPragma is the reason this package exists. The modernc
// driver silently ignores the mattn-style `_busy_timeout=` spelling, so a DSN
// can look right, open fine, and leave the pragma at its default. Assert the
// pragmas as the database actually reports them.
func TestOpen_AppliesEveryPragma(t *testing.T) {
	db, _ := mustOpen(t, sqlitex.Options{})

	if got := queryInt(t, db, "PRAGMA busy_timeout"); got != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", got)
	}
	if got := queryString(t, db, "PRAGMA journal_mode"); got != "wal" {
		t.Errorf("journal_mode = %q, want wal", got)
	}
	if got := queryInt(t, db, "PRAGMA foreign_keys"); got != 1 {
		t.Errorf("foreign_keys = %d, want 1 (a declared ON DELETE CASCADE no-ops when this is off)", got)
	}
	// synchronous: 2 = FULL, 1 = NORMAL.
	if got := queryInt(t, db, "PRAGMA synchronous"); got != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL) by default", got)
	}
}

func TestOpen_BusyTimeoutIsConfigurable(t *testing.T) {
	db, _ := mustOpen(t, sqlitex.Options{BusyTimeout: 250 * time.Millisecond})
	if got := queryInt(t, db, "PRAGMA busy_timeout"); got != 250 {
		t.Errorf("busy_timeout = %d, want 250", got)
	}
}

func TestOpen_SyncNormal(t *testing.T) {
	db, _ := mustOpen(t, sqlitex.Options{Sync: sqlitex.SyncNormal})
	if got := queryInt(t, db, "PRAGMA synchronous"); got != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", got)
	}
}

// TestOpen_PragmasApplyToEveryPooledConnection is the property the DSN form
// buys. A post-open `db.Exec("PRAGMA ...")` configures only the connection that
// served it; with several connections in flight the others keep the defaults.
func TestOpen_PragmasApplyToEveryPooledConnection(t *testing.T) {
	db, _ := mustOpen(t, sqlitex.Options{})

	// Hold several connections open simultaneously so the pool is forced to
	// create more than one, then check each reports the configured pragmas.
	const conns = 4
	held := make([]*sql.Conn, 0, conns)
	for range conns {
		c, err := db.Conn(t.Context())
		if err != nil {
			t.Fatalf("acquiring connection: %v", err)
		}
		held = append(held, c)
	}
	for i, c := range held {
		var bt, fk int
		if err := c.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&bt); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i, err)
		}
		if err := c.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("conn %d foreign_keys: %v", i, err)
		}
		if bt != 5000 || fk != 1 {
			t.Errorf("connection %d has busy_timeout=%d foreign_keys=%d, want 5000/1", i, bt, fk)
		}
		_ = c.Close()
	}
}

func TestOpen_CreatesDirectoryPrivately(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	_, path := mustOpen(t, sqlitex.Options{})
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("database directory mode = %04o, want 0700", got)
	}
}

func TestOpenReadOnly_AppliesBusyTimeoutAndRefusesWrites(t *testing.T) {
	db, path := mustOpen(t, sqlitex.Options{})
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := sqlitex.OpenReadOnly(path, sqlitex.ReadOnlyOptions{BusyTimeout: 1200 * time.Millisecond})
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	// The bug this guards: cmd/clientsmoke still used the mattn spelling, which
	// left a read-only handle at busy_timeout=0.
	if got := queryInt(t, ro, "PRAGMA busy_timeout"); got != 1200 {
		t.Errorf("busy_timeout = %d, want 1200", got)
	}
	if _, err := ro.Exec(`INSERT INTO t (id) VALUES (1)`); err == nil {
		t.Error("insert succeeded on a mode=ro handle, want a failure")
	}
}

func TestVersion_AndStampVersion(t *testing.T) {
	db, _ := mustOpen(t, sqlitex.Options{})

	got, err := sqlitex.Version(db)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != 0 {
		t.Errorf("fresh database version = %d, want 0", got)
	}

	if err := sqlitex.StampVersion(db, 7); err != nil {
		t.Fatalf("StampVersion: %v", err)
	}
	got, err = sqlitex.Version(db)
	if err != nil {
		t.Fatalf("Version after stamp: %v", err)
	}
	if got != 7 {
		t.Errorf("version = %d after stamping 7", got)
	}
}

// TestOpen_ForeignKeysActuallyCascade proves foreign_keys=1 is doing work,
// rather than just reading back as 1.
func TestOpen_ForeignKeysActuallyCascade(t *testing.T) {
	db, _ := mustOpen(t, sqlitex.Options{})

	if _, err := db.Exec(`
CREATE TABLE parent (id INTEGER PRIMARY KEY);
CREATE TABLE child (
    id        INTEGER PRIMARY KEY,
    parent_id INTEGER NOT NULL REFERENCES parent(id) ON DELETE CASCADE
);
INSERT INTO parent (id) VALUES (1);
INSERT INTO child (id, parent_id) VALUES (10, 1);
`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM parent WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, db, "SELECT COUNT(*) FROM child"); n != 0 {
		t.Errorf("child rows after cascading delete = %d, want 0 — foreign_keys is not being enforced", n)
	}
}

// TestOpen_HandlesAwkwardPaths pins the escaping. The DSN is a file: URI, so a
// workspace path containing a space, a '#' or a '?' has to survive
// percent-encoding intact — otherwise the database silently lands somewhere
// other than where the caller asked for it, or fails to open at all.
func TestOpen_HandlesAwkwardPaths(t *testing.T) {
	for _, name := range []string{
		"plain", "with space", "with#hash", "with?question", "with%percent", "wïth-ünicode",
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "t.db")

			db, err := sqlitex.Open(path, sqlitex.Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
				t.Fatalf("create: %v", err)
			}
			if got := queryInt(t, db, "PRAGMA busy_timeout"); got != 5000 {
				t.Errorf("busy_timeout = %d, want 5000", got)
			}
			_ = db.Close()

			// The file must exist exactly where it was asked for, not at some
			// path the URI encoding mangled.
			if _, err := os.Stat(path); err != nil {
				t.Errorf("database not at %s: %v", path, err)
			}

			ro, err := sqlitex.OpenReadOnly(path, sqlitex.ReadOnlyOptions{})
			if err != nil {
				t.Fatalf("OpenReadOnly: %v", err)
			}
			defer ro.Close()
			if _, err := ro.Exec(`INSERT INTO t (id) VALUES (1)`); err == nil {
				t.Error("write succeeded on a read-only handle")
			}
		})
	}
}
