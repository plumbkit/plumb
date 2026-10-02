package history

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sqlitex"
)

// SchemaVersion is history.db's PRAGMA user_version.
//
//	1 — workspaces, paths, changes, meta (spec §4)
const SchemaVersion = 1

// ErrNewerSchema means history.db was written by a newer plumb. Recording is
// disabled rather than risk writing rows an older binary misunderstands.
var ErrNewerSchema = errors.New("history: database schema is newer than this plumb")

// DBPath is the global history database, beside stats.db.
func DBPath() string { return filepath.Join(config.DataDir(), "history.db") }

const ddl = `
CREATE TABLE IF NOT EXISTS workspaces (
  id   INTEGER PRIMARY KEY,
  root TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS paths (
  id           INTEGER PRIMARY KEY,
  workspace_id INTEGER NOT NULL REFERENCES workspaces(id),
  path         TEXT NOT NULL,
  UNIQUE (workspace_id, path)
);
CREATE TABLE IF NOT EXISTS changes (
  seq           INTEGER PRIMARY KEY AUTOINCREMENT,
  ts_ms         INTEGER NOT NULL,
  call_id       TEXT    NOT NULL,
  workspace_id  INTEGER NOT NULL REFERENCES workspaces(id),
  path_id       INTEGER NOT NULL REFERENCES paths(id),
  from_path_id  INTEGER REFERENCES paths(id),
  kind          TEXT    NOT NULL DEFAULT 'file' CHECK (kind IN ('file','dir')),
  op            TEXT    NOT NULL CHECK (op IN ('create','update','delete','rename','copy','revert')),
  tool          TEXT    NOT NULL,
  session_id    TEXT    NOT NULL DEFAULT '',
  session_name  TEXT    NOT NULL DEFAULT '',
  logical_agent TEXT    NOT NULL DEFAULT '',
  client_name   TEXT    NOT NULL DEFAULT '',
  before_sha    BLOB,
  after_sha     BLOB,
  before_size   INTEGER,
  after_size    INTEGER,
  added         INTEGER NOT NULL DEFAULT 0,
  removed       INTEGER NOT NULL DEFAULT 0,
  content       TEXT    NOT NULL CHECK (content IN
                  ('diff','none','withheld:sensitive','withheld:binary','withheld:too_large','withheld:overflow')),
  redactions    INTEGER NOT NULL DEFAULT 0,
  diff          BLOB,
  reverts_seq   INTEGER REFERENCES changes(seq) ON DELETE SET NULL,
  reason        TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_ch_ws_ts   ON changes(workspace_id, ts_ms);
CREATE INDEX IF NOT EXISTS idx_ch_path_ts ON changes(path_id, ts_ms);
CREATE INDEX IF NOT EXISTS idx_ch_sess_ts ON changes(session_id, ts_ms);
CREATE INDEX IF NOT EXISTS idx_ch_call    ON changes(call_id);
CREATE INDEX IF NOT EXISTS idx_ch_ts_seq  ON changes(ts_ms, seq);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
` + fkIndexes

// fkIndexes index the two columns that REFERENCE changes. With foreign keys on,
// deleting a change scans for rows pointing at it; without these, prune is
// quadratic (measured 34 s for 20k rows, 135 ms with them) and holds the write
// lock against the daemon the whole time. They are created on every
// read-write open rather than by a version bump: a file written before them
// still reads as v1, so an older plumb keeps reading and writing it.
const fkIndexes = `
CREATE INDEX IF NOT EXISTS idx_ch_reverts ON changes(reverts_seq);
CREATE INDEX IF NOT EXISTS idx_ch_from    ON changes(from_path_id);
`

// initSchema creates the schema on a fresh database and stamps v1; accepts v1;
// refuses anything newer with ErrNewerSchema.
func initSchema(db *sql.DB) error {
	v, err := sqlitex.Version(db)
	if err != nil {
		return fmt.Errorf("history: read version: %w", err)
	}
	if v > SchemaVersion {
		return fmt.Errorf("%w (file v%d, this plumb v%d)", ErrNewerSchema, v, SchemaVersion)
	}
	if v == SchemaVersion {
		if _, err := db.Exec(fkIndexes); err != nil {
			return fmt.Errorf("history: create foreign-key indexes: %w", err)
		}
		return nil
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("history: create schema: %w", err)
	}
	return sqlitex.StampVersion(db, SchemaVersion)
}
