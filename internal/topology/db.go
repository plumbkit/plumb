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
    language   TEXT    NOT NULL DEFAULT '',
    -- Byte-precise declaration span (see topology.Node). has_bytes=0 ⇒ the
    -- byte/column columns are absent and consumers fall back to the line range.
    has_bytes      INTEGER NOT NULL DEFAULT 0,
    start_byte     INTEGER NOT NULL DEFAULT 0,
    end_byte       INTEGER NOT NULL DEFAULT 0,
    start_col      INTEGER NOT NULL DEFAULT 0,
    end_col        INTEGER NOT NULL DEFAULT 0,
    -- Optional doc-comment byte span; present only when doc_end_byte > doc_start_byte.
    doc_start_byte INTEGER NOT NULL DEFAULT 0,
    doc_end_byte   INTEGER NOT NULL DEFAULT 0
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

-- Opt-in semantic-search cache: one row per (embedding model, content hash).
-- Keyed by content hash (not node id) so it survives re-indexing and shares a
-- vector across identical symbol text. Populated lazily when a symbol appears as
-- a topology_search rerank candidate. vector is dim little-endian float32s.
CREATE TABLE IF NOT EXISTS topology_embeddings (
    model        TEXT    NOT NULL,
    content_hash TEXT    NOT NULL,
    dim          INTEGER NOT NULL,
    vector       BLOB    NOT NULL,
    PRIMARY KEY (model, content_hash)
);
`

// SchemaVersion is the current on-disk topology schema version, persisted in
// PRAGMA user_version. topology.db is a REBUILDABLE index, so there is no
// data-preserving migration path: when the on-disk version is older, the
// topology tables are DROPped and recreated, and the indexer repopulates them
// on the resync that runs at every attach.
//
// History:
//
//	0 — pre-versioned (every release up to 0.9.21)
//	1 — added byte/column declaration spans + doc-comment spans to topology_nodes
const SchemaVersion = 1

// topologyTables are the topology tables/virtual tables, listed so the version
// gate can DROP them in dependency order (children before parents). The
// embeddings cache is rebuildable too but is keyed by content hash, not node id,
// so it is intentionally preserved across a schema recreate.
var topologyTables = []string{
	"topology_edges",
	"topology_fts",
	"topology_nodes",
	"topology_files",
	"topology_meta",
}

// DBPath returns the canonical path to the topology database for the given workspace.
func DBPath(workspace string) string {
	return filepath.Join(workspace, ".plumb", "topology.db")
}

// dbDSNParams configures EVERY pooled connection at open time. busy_timeout and
// foreign_keys are per-connection SQLite pragmas, so they must travel in the DSN:
// a one-off db.Exec sets them on only the single connection that served it,
// leaving every other pooled connection with foreign_keys OFF (ON DELETE CASCADE
// silently no-ops, so orphan topology_edges accumulate on every re-index/prune)
// and busy_timeout 0 (recoverable writer contention becomes an immediate
// "database is locked"). The modernc driver applies _pragma= params on each new
// connection.
const dbDSNParams = "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"

// openDB opens or creates the topology SQLite database at path with WAL mode,
// busy timeout, and foreign-key enforcement set per-connection via the DSN, then
// applies the schema. Returns a ready-to-use *sql.DB.
func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("topology: create db dir: %w", err)
	}
	if err := ensureGitignore(filepath.Dir(path)); err != nil {
		slog.Warn("topology: ensure .gitignore", "dir", filepath.Dir(path), "err", err)
	}
	db, err := sql.Open("sqlite", path+dbDSNParams)
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
	// WAL / busy_timeout / foreign_keys are set per-connection via dbDSNParams so
	// they apply to every pooled connection, not just the one a db.Exec would hit.
	if err := ensureSchemaVersion(db); err != nil {
		return err
	}
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("topology: apply schema: %w", err)
	}
	return nil
}

// ensureSchemaVersion gates the rebuildable topology schema on PRAGMA
// user_version. A fresh database (version 0 with no node table) is simply
// stamped at the current version; an existing database stamped below the current
// version has its topology tables dropped and recreated (the indexer repopulates
// them on the next resync). The DROP must precede the CREATE-IF-NOT-EXISTS
// schema, because CREATE TABLE IF NOT EXISTS never alters a table that already
// has the old column set — an INSERT naming the new columns would then fail.
func ensureSchemaVersion(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("topology: reading user_version: %w", err)
	}
	if version >= SchemaVersion {
		return nil
	}
	if version > 0 || hasNodesTable(db) {
		for _, t := range topologyTables {
			if _, err := db.Exec(`DROP TABLE IF EXISTS ` + t); err != nil { //nolint:gosec // G202: t is a fixed package constant, never user data
				return fmt.Errorf("topology: dropping %s for schema upgrade: %w", t, err)
			}
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion)); err != nil {
		return fmt.Errorf("topology: stamping user_version: %w", err)
	}
	return nil
}

// hasNodesTable reports whether a topology_nodes table already exists, used to
// distinguish a brand-new database (no tables, version 0) from a pre-versioned
// one carrying the old column set.
func hasNodesTable(db *sql.DB) bool {
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='topology_nodes'`).Scan(&name)
	return err == nil
}
