// Package sessionstate persists the small slice of per-connection daemon state
// that must survive a daemon restart so a continuously-connected agent keeps
// working transparently.
//
// When `plumb daemon` restarts, the resilient `plumb serve` proxy stays connected
// to the agent and replays the captured MCP `initialize` handshake. The proxy
// injects a stable per-proxy session ID into that handshake, identical across
// every replay, which lets the fresh daemon recognise the reconnected connection
// as a continuation of the previous one and rehydrate its state.
//
// Two pieces of state are persisted, keyed by that proxy session ID:
//
//   - read-tracking records (path → mtime + content SHA), scoped by workspace, so
//     strict-mode "must read before edit" survives a restart;
//   - the pinned workspace root, so a client that does not report roots/list
//     (e.g. Claude Desktop) comes back pinned without an explicit session_start.
//
// This is deliberately a separate SQLite database from stats.db: stats is
// append-only metrics whose writer drops on overflow by design, which would
// silently corrupt a strict-read record; this store needs synchronous,
// durable upserts and an independent lifecycle (its own schema version, TTL
// pruning, and WAL).
//
// WAL journal mode lets the daemon (writer) and any reader operate from
// different OS processes without blocking.
package sessionstate

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/plumbkit/plumb/internal/config"
)

// schema is the v1 baseline shape, deliberately FROZEN. read_tracking is scoped
// by (proxy_session_id, workspace) so a reconnected connection can never
// resurrect reads for a different project; pinned_workspace records the last
// root pinned under a given proxy session. mtime is stored as Unix nanoseconds
// to preserve the exact time.Time.Equal comparison the strict-read guard
// performs.
//
// Every column added after v1 belongs in migrate, never here. A fresh database
// starts at user_version=0, so it runs the same migrations as an existing file;
// declaring a migrated column here too would make its ALTER fail with
// "duplicate column name" on every fresh open.
const schema = `
CREATE TABLE IF NOT EXISTS read_tracking (
    proxy_session_id TEXT    NOT NULL,
    workspace        TEXT    NOT NULL,
    path             TEXT    NOT NULL,
    mtime_unix_nano  INTEGER NOT NULL,
    sha              TEXT    NOT NULL DEFAULT '',
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (proxy_session_id, workspace, path)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_rt_updated ON read_tracking(updated_at);

CREATE TABLE IF NOT EXISTS pinned_workspace (
    proxy_session_id TEXT    PRIMARY KEY,
    workspace        TEXT    NOT NULL,
    language         TEXT    NOT NULL DEFAULT '',
    updated_at       INTEGER NOT NULL
);
`

// SchemaVersion is the current on-disk schema version, persisted in PRAGMA
// user_version on every Open. Open reads the on-disk version, applies any
// pending migrations, then stamps the new version.
//
// History:
//
//	1 — initial schema: read_tracking + pinned_workspace
//	2 — pinned_workspace.source: why the workspace was pinned
const SchemaVersion = 2

// PinSource records WHY a workspace was pinned. It is the discriminator that
// lets a reconnecting connection tell a deliberate re-pin from a stale copy of
// the client's roots: only PinSourceSessionStart outranks a fresh roots/list
// answer, because only it represents a workspace the caller actually chose.
//
// A row written before this column existed reads as PinSourceUnknown, which
// does not outrank roots — so upgrading changes no behaviour until the next
// deliberate re-pin.
type PinSource string

const (
	// PinSourceUnknown is a legacy row, or a pin that must not be persisted.
	PinSourceUnknown PinSource = ""
	// PinSourceRoots came from the client's roots/list answer.
	PinSourceRoots PinSource = "roots"
	// PinSourceSessionStart came from an explicit session_start workspace arg.
	PinSourceSessionStart PinSource = "session_start"
)

// ReadRecord is one persisted read-tracking entry for a path: the mtime and
// content SHA-256 read_file observed. It mirrors the in-memory record the
// ReadTracker hydrates from.
type ReadRecord struct {
	Path  string
	Mtime time.Time
	SHA   string
}

// Store is a thread-safe persistence store for per-connection daemon state,
// backed by SQLite. Concurrency: all methods are safe for concurrent use.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// DBPath returns the session-state database path in the persistent data
// directory, a sibling of stats.db.
func DBPath() string {
	return filepath.Join(config.DataDir(), "session_state.db")
}

// Open opens (or creates) the session-state database at the conventional global
// path, creating the parent directory and applying any pending migrations.
func Open() (*Store, error) {
	return openAt(DBPath())
}

// openAt opens (or creates) the session-state database at an explicit path. Open
// delegates here; tests open at a temp path.
func openAt(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("sessionstate: mkdir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("sessionstate: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	// synchronous=NORMAL is corruption-safe under WAL and avoids an fsync per
	// commit. WAL + busy_timeout come from the DSN via the `_pragma=` form — the
	// modernc driver SILENTLY IGNORES the mattn-style `_busy_timeout=`/
	// `_journal_mode=` params — and synchronous is asserted here since it is
	// per-connection.
	if _, err := db.Exec("PRAGMA synchronous = NORMAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("sessionstate: synchronous: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sessionstate: schema: %w", err)
	}
	current, err := readVersion(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db, current); err != nil {
		db.Close()
		return nil, err
	}
	if err := stampVersion(db, current); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// readVersion reads the on-disk schema version. A database this process just
// created reads 0; one written by an older plumb reads its own version.
func readVersion(db *sql.DB) (int, error) {
	var current int
	if err := db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return 0, fmt.Errorf("sessionstate: reading user_version: %w", err)
	}
	return current, nil
}

// migrate brings a database at version `from` up to SchemaVersion. Each step is
// gated on the on-disk version, so it runs exactly once per database and a
// re-open is a no-op. The baseline `schema` above is frozen at v1, so a fresh
// database (version 0) and an upgraded one converge on the same shape here.
func migrate(db *sql.DB, from int) error {
	if from < 2 {
		// SQLite permits a NOT NULL column via ADD COLUMN when it carries a
		// default, which back-fills every pre-existing row to the unknown origin.
		const addSource = `ALTER TABLE pinned_workspace ADD COLUMN source TEXT NOT NULL DEFAULT ''`
		if _, err := db.Exec(addSource); err != nil {
			return fmt.Errorf("sessionstate: migrate v2 (pinned_workspace.source): %w", err)
		}
	}
	return nil
}

// stampVersion records SchemaVersion once the migrations for `current` have run.
// The stamp is a write, so only issue it when the version actually moved.
func stampVersion(db *sql.DB, current int) error {
	if current >= SchemaVersion {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("sessionstate: stamping user_version: %w", err)
	}
	return nil
}

// Close closes the database. nil-safe.
func (s *Store) Close() {
	if s != nil && s.db != nil {
		_ = s.db.Close()
	}
}

// UpsertRead records the mtime and content SHA read_file observed for a path,
// scoped by (proxySessionID, workspace). nil-safe; a no-op when any key field is
// empty (an unidentified proxy or unpinned workspace cannot be rehydrated).
func (s *Store) UpsertRead(proxySessionID, workspace, path string, mtime time.Time, sha string) error {
	if s == nil || proxySessionID == "" || workspace == "" || path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO read_tracking (proxy_session_id, workspace, path, mtime_unix_nano, sha, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(proxy_session_id, workspace, path)
		 DO UPDATE SET mtime_unix_nano=excluded.mtime_unix_nano, sha=excluded.sha, updated_at=excluded.updated_at`,
		proxySessionID, workspace, path, mtime.UnixNano(), sha, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("sessionstate: upsert read: %w", err)
	}
	return nil
}

// LoadReads returns every persisted read record for (proxySessionID, workspace).
// nil-safe; returns nil when any key field is empty.
func (s *Store) LoadReads(proxySessionID, workspace string) ([]ReadRecord, error) {
	if s == nil || proxySessionID == "" || workspace == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT path, mtime_unix_nano, sha FROM read_tracking
		 WHERE proxy_session_id=? AND workspace=?`,
		proxySessionID, workspace,
	)
	if err != nil {
		return nil, fmt.Errorf("sessionstate: load reads: %w", err)
	}
	defer rows.Close()
	var out []ReadRecord
	for rows.Next() {
		var path, sha string
		var nano int64
		if err := rows.Scan(&path, &nano, &sha); err != nil {
			return nil, fmt.Errorf("sessionstate: scan read: %w", err)
		}
		out = append(out, ReadRecord{Path: path, Mtime: time.Unix(0, nano), SHA: sha})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessionstate: load reads: %w", err)
	}
	return out, nil
}

// UpsertPin records the workspace root (and primary language) pinned under a
// proxy session, so a client that does not report roots comes back pinned after
// a restart. source records why it was pinned — see PinSource. nil-safe; a no-op
// when proxySessionID or workspace is empty.
func (s *Store) UpsertPin(proxySessionID, workspace, language string, source PinSource) error {
	if s == nil || proxySessionID == "" || workspace == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO pinned_workspace (proxy_session_id, workspace, language, source, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(proxy_session_id)
		 DO UPDATE SET workspace=excluded.workspace, language=excluded.language,
		               source=excluded.source, updated_at=excluded.updated_at`,
		proxySessionID, workspace, language, string(source), time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("sessionstate: upsert pin: %w", err)
	}
	return nil
}

// LoadPin returns the workspace root, language, and origin pinned under
// proxySessionID. ok is false when no pin is recorded. A row written before the
// source column existed reads as PinSourceUnknown. nil-safe (returns ok=false).
func (s *Store) LoadPin(proxySessionID string) (workspace, language string, source PinSource, ok bool, err error) {
	if s == nil || proxySessionID == "" {
		return "", "", PinSourceUnknown, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(
		`SELECT workspace, language, source FROM pinned_workspace WHERE proxy_session_id=?`,
		proxySessionID,
	)
	var src string
	switch err := row.Scan(&workspace, &language, &src); err {
	case nil:
		return workspace, language, PinSource(src), true, nil
	case sql.ErrNoRows:
		return "", "", PinSourceUnknown, false, nil
	default:
		return "", "", PinSourceUnknown, false, fmt.Errorf("sessionstate: load pin: %w", err)
	}
}

// Prune deletes all persisted state last updated before olderThan, reclaiming
// rows left behind by a `plumb serve` that died without reconnecting. nil-safe.
func (s *Store) Prune(olderThan time.Time) error {
	if s == nil {
		return nil
	}
	cutoff := olderThan.UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM read_tracking WHERE updated_at < ?`, cutoff); err != nil {
		return fmt.Errorf("sessionstate: prune reads: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM pinned_workspace WHERE updated_at < ?`, cutoff); err != nil {
		return fmt.Errorf("sessionstate: prune pins: %w", err)
	}
	return nil
}
