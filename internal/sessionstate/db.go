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
// Three pieces of state are persisted, keyed by that proxy session ID:
//
//   - read-tracking records (path → mtime + content SHA), scoped by workspace, so
//     strict-mode "must read before edit" survives a restart;
//   - the pinned workspace root, so a client that does not report roots/list
//     (e.g. Claude Desktop) comes back pinned without an explicit session_start;
//   - the session name, so a reconnect keeps the same name and mailbox notes
//     addressed to it (delivery matches on the name string) stay deliverable.
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
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sqlitex"
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
//	3 — session_names: the session name recorded under a proxy session ID
//	4 — session_names.plumb_session_id: the session ID that name belonged to,
//	    so a reconnect can inherit its predecessor's mailbox identity
const SchemaVersion = 4

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
	// SyncNormal: this is derived session bookkeeping, cheap to lose and
	// rewritten constantly, so a per-commit fsync is not worth its cost.
	db, err := sqlitex.Open(path, sqlitex.Options{Sync: sqlitex.SyncNormal, MaxOpenConns: 1})
	if err != nil {
		return nil, fmt.Errorf("sessionstate: open %s: %w", path, err)
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
	return sqlitex.Version(db)
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
	if from < 3 {
		// A dedicated table, not a column on pinned_workspace: a pin row only
		// exists once a workspace is pinned, but the name must be recorded for
		// every identified proxy session, pinned or not.
		const addNames = `CREATE TABLE IF NOT EXISTS session_names (
    proxy_session_id TEXT    PRIMARY KEY,
    name             TEXT    NOT NULL,
    updated_at       INTEGER NOT NULL
)`
		if _, err := db.Exec(addNames); err != nil {
			return fmt.Errorf("sessionstate: migrate v3 (session_names): %w", err)
		}
	}
	if from < 4 {
		// The plumb session ID the name belonged to, so a reconnecting proxy can
		// inherit its predecessor's mailbox identity and collect messages bound to
		// it. A pre-v4 row back-fills to "" and inherits nothing, which is the
		// behaviour that shipped before this column existed.
		const addSessionID = `ALTER TABLE session_names ADD COLUMN plumb_session_id TEXT NOT NULL DEFAULT ''`
		if _, err := db.Exec(addSessionID); err != nil {
			return fmt.Errorf("sessionstate: migrate v4 (session_names.plumb_session_id): %w", err)
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
	return sqlitex.StampVersion(db, SchemaVersion)
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

// DeletePin removes the pin recorded under proxySessionID. Used when a
// persisted pin no longer verifies at restore time (its directory is gone, or
// it now resolves to a different root): leaving the row would re-attempt the
// same drop on every reconnect and every unpinned tool call. nil-safe; a no-op
// when proxySessionID is empty or no row exists.
func (s *Store) DeletePin(proxySessionID string) error {
	if s == nil || proxySessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM pinned_workspace WHERE proxy_session_id=?`, proxySessionID); err != nil {
		return fmt.Errorf("sessionstate: delete pin: %w", err)
	}
	return nil
}

// Identity is what a proxy session was last known as: the display name it
// answered to, and the plumb session ID that name belonged to.
//
// The two travel together because a reconnect needs both. The name is what peers
// address; the session ID is what a message addressed to that name is BOUND to,
// so a reconnecting proxy has to present its predecessor's ID to collect mail
// written before the restart. Storing the ID beside the name is what makes that
// inheritance provable rather than assumed from the name alone.
type Identity struct {
	// Name is the session's display name.
	Name string
	// SessionID is the plumb session ID that held Name. Empty on a row written
	// before this column existed, which inherits nothing.
	SessionID string
}

// SaveIdentity records the session name and its plumb session ID under a proxy
// session ID, so a reconnect after a daemon restart comes back under the same
// name and can prove which session it continues. nil-safe; a no-op when
// proxySessionID or name is empty. sessionID may be empty (an unregistered
// session has no identity to hand on).
func (s *Store) SaveIdentity(proxySessionID, name, sessionID string) error {
	if s == nil || proxySessionID == "" || name == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO session_names (proxy_session_id, name, plumb_session_id, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(proxy_session_id)
		 DO UPDATE SET name=excluded.name, plumb_session_id=excluded.plumb_session_id,
		               updated_at=excluded.updated_at`,
		proxySessionID, name, sessionID, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("sessionstate: save identity: %w", err)
	}
	return nil
}

// LoadIdentity returns the identity recorded under proxySessionID. ok is false
// when none is recorded. nil-safe (returns ok=false).
//
// A row written before the plumb_session_id column existed returns an empty
// SessionID, so an upgraded daemon restores the name exactly as it always did
// and inherits nothing — the caller must treat an empty SessionID as "no
// predecessor", never as a wildcard.
func (s *Store) LoadIdentity(proxySessionID string) (id Identity, ok bool, err error) {
	if s == nil || proxySessionID == "" {
		return Identity{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(
		`SELECT name, plumb_session_id FROM session_names WHERE proxy_session_id=?`,
		proxySessionID,
	)
	switch err := row.Scan(&id.Name, &id.SessionID); err {
	case nil:
		return id, true, nil
	case sql.ErrNoRows:
		return Identity{}, false, nil
	default:
		return Identity{}, false, fmt.Errorf("sessionstate: load identity: %w", err)
	}
}

// liveExemption builds the "AND proxy_session_id NOT IN (?, ?, …)" tail that
// spares live sessions from the TTL sweep, plus the full argument list for the
// DELETE (cutoff first). Returns an empty tail when no session is live, so the
// statement stays exactly as it was.
func liveExemption(cutoff int64, live []string) (string, []any) {
	args := make([]any, 0, 1+len(live))
	args = append(args, cutoff)
	if len(live) == 0 {
		return "", args
	}
	var b strings.Builder
	b.WriteString(" AND proxy_session_id NOT IN (")
	for i, id := range live {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("?")
		args = append(args, id)
	}
	b.WriteString(")")
	return b.String(), args
}

// Prune deletes all persisted state last updated before olderThan, reclaiming
// rows left behind by a `plumb serve` that died without reconnecting. nil-safe.
//
// Rows belonging to a proxy session in live are kept regardless of age. Without
// that exemption the sweep reclaims state from sessions that are still
// connected: read rows are refreshed as the session works, but the pin and the
// name are written once at initialize, so any conversation older than the TTL
// (24 h by default) loses them mid-flight and its next reconnect comes back
// unpinned and renamed — the very churn persistence exists to prevent.
func (s *Store) Prune(olderThan time.Time, live ...string) error {
	if s == nil {
		return nil
	}
	cutoff := olderThan.UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	// keep is built only from "?" placeholders (liveExemption), never from a
	// caller-supplied string; the proxy session IDs travel as bound arguments.
	keep, args := liveExemption(cutoff, live)
	if _, err := s.db.Exec(`DELETE FROM read_tracking WHERE updated_at < ?`+keep, args...); err != nil { //nolint:gosec // G202: keep is a placeholder-only fragment, IDs are bound args
		return fmt.Errorf("sessionstate: prune reads: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM pinned_workspace WHERE updated_at < ?`+keep, args...); err != nil { //nolint:gosec // G202: keep is a placeholder-only fragment, IDs are bound args
		return fmt.Errorf("sessionstate: prune pins: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM session_names WHERE updated_at < ?`+keep, args...); err != nil { //nolint:gosec // G202: keep is a placeholder-only fragment, IDs are bound args
		return fmt.Errorf("sessionstate: prune names: %w", err)
	}
	return nil
}
