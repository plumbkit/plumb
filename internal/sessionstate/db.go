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

// schema is the current fresh-database shape. read_tracking is scoped by
// (proxy_session_id, workspace) so a reconnected connection can never resurrect
// reads for a different project; pinned_workspace records the last root pinned
// under a given proxy session. mtime is stored as Unix nanoseconds to preserve
// the exact time.Time.Equal comparison the strict-read guard performs.
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
const SchemaVersion = 1

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
	if err := stampVersion(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// stampVersion reads the on-disk user_version and, when behind, stamps it.
// There are no migrations yet (v1 is the initial schema); the stamp is a write,
// so only issue it when the version actually moved.
func stampVersion(db *sql.DB) error {
	var current int
	if err := db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("sessionstate: reading user_version: %w", err)
	}
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
// a restart. nil-safe; a no-op when proxySessionID or workspace is empty.
func (s *Store) UpsertPin(proxySessionID, workspace, language string) error {
	if s == nil || proxySessionID == "" || workspace == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO pinned_workspace (proxy_session_id, workspace, language, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(proxy_session_id)
		 DO UPDATE SET workspace=excluded.workspace, language=excluded.language, updated_at=excluded.updated_at`,
		proxySessionID, workspace, language, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("sessionstate: upsert pin: %w", err)
	}
	return nil
}

// LoadPin returns the workspace root and language pinned under proxySessionID.
// ok is false when no pin is recorded. nil-safe (returns ok=false).
func (s *Store) LoadPin(proxySessionID string) (workspace, language string, ok bool, err error) {
	if s == nil || proxySessionID == "" {
		return "", "", false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(
		`SELECT workspace, language FROM pinned_workspace WHERE proxy_session_id=?`,
		proxySessionID,
	)
	switch err := row.Scan(&workspace, &language); err {
	case nil:
		return workspace, language, true, nil
	case sql.ErrNoRows:
		return "", "", false, nil
	default:
		return "", "", false, fmt.Errorf("sessionstate: load pin: %w", err)
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
