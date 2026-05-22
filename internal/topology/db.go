package topology

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // register the SQLite driver
)

const schema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS topology_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS topology_files (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    path          TEXT    NOT NULL UNIQUE,
    language      TEXT    NOT NULL DEFAULT '',
    mtime_ns      INTEGER NOT NULL DEFAULT 0,
    content_hash  TEXT    NOT NULL DEFAULT '',
    extractor_ver TEXT    NOT NULL DEFAULT '',
    indexed_at    INTEGER NOT NULL DEFAULT 0,
    error_msg     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tf_path ON topology_files(path);

CREATE TABLE IF NOT EXISTS topology_nodes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id    INTEGER NOT NULL REFERENCES topology_files(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL,
    name       TEXT    NOT NULL DEFAULT '',
    qualified  TEXT    NOT NULL DEFAULT '',
    signature  TEXT    NOT NULL DEFAULT '',
    start_line INTEGER NOT NULL DEFAULT 0,
    end_line   INTEGER NOT NULL DEFAULT 0,
    docstring  TEXT    NOT NULL DEFAULT '',
    language   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tn_file ON topology_nodes(file_id);
CREATE INDEX IF NOT EXISTS idx_tn_name ON topology_nodes(name);
CREATE INDEX IF NOT EXISTS idx_tn_kind ON topology_nodes(kind);

CREATE TABLE IF NOT EXISTS topology_edges (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id    INTEGER NOT NULL REFERENCES topology_nodes(id) ON DELETE CASCADE,
    to_id      INTEGER NOT NULL REFERENCES topology_nodes(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL,
    confidence REAL    NOT NULL DEFAULT 1.0,
    source     TEXT    NOT NULL DEFAULT 'extractor'
);
CREATE INDEX IF NOT EXISTS idx_te_from ON topology_edges(from_id);
CREATE INDEX IF NOT EXISTS idx_te_to   ON topology_edges(to_id);

CREATE VIRTUAL TABLE IF NOT EXISTS topology_fts USING fts5(
    name,
    name_tokens,
    qualified,
    signature,
    docstring,
    path,
    kind,
    tokenize='unicode61 remove_diacritics 2'
);
`

// DBPath returns the canonical path to the topology database for the given workspace.
func DBPath(workspace string) string {
	return filepath.Join(workspace, ".plumb", "topology.db")
}

// openDB opens or creates the topology SQLite database at path, sets WAL mode,
// busy timeout, and applies the schema. Returns a ready-to-use *sql.DB.
func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("topology: create db dir: %w", err)
	}
	if err := ensureGitignore(filepath.Dir(path)); err != nil {
		slog.Warn("topology: ensure .gitignore", "dir", filepath.Dir(path), "err", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("topology: open db: %w", err)
	}
	if err := initDB(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ensureGitignore makes sure dir/.gitignore excludes the topology database and
// its SQLite sidecar files. The index is a rebuildable, high-churn binary that
// must never be committed, even in workspaces that deliberately track .plumb/.
// Idempotent: it appends only the entries that are missing and is a no-op once
// they are all present. Best-effort — the caller logs and continues on error.
func ensureGitignore(dir string) error {
	const header = "# plumb topology index (rebuildable; do not commit)"
	entries := []string{"topology.db", "topology.db-wal", "topology.db-shm"}

	path := filepath.Join(dir, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read .gitignore: %w", err)
	}

	have := make(map[string]bool)
	for line := range strings.SplitSeq(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}

	var missing []string
	for _, e := range entries {
		if !have[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteByte('\n')
	}
	if !have[header] {
		b.WriteString(header)
		b.WriteByte('\n')
	}
	for _, e := range missing {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644) //nolint:gosec // G306: .gitignore is a normal repo file; 0644 is intentional
}

func initDB(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("topology: WAL mode: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return fmt.Errorf("topology: busy_timeout: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("topology: foreign_keys: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("topology: apply schema: %w", err)
	}
	return nil
}
