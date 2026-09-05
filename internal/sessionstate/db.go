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
//	5 — meta: a tiny key/value table for one-shot maintenance flags, so a
//	    sweep that must run exactly once per database can record that it did
//	6 — logical_agent_id on read_tracking + pinned_workspace: a shared connection
//	    persists each logical agent's reads and pin independently (PLAN-286)
//	7 — session_names.external_id + session_names.name_revision: the canonical
//	    durable identity record carries its own authorised external linkage (so
//	    recovery no longer depends on a prunable ended-session JSON file) and a
//	    revision that orders name updates (PLAN-426)
const SchemaVersion = 7

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
	if from < 5 {
		// A key/value side table rather than more columns: these are facts about
		// the DATABASE (which one-shot maintenance has already run), not about any
		// proxy session. The schema version is the wrong place to record "has this
		// run yet", because that answer has to survive later version bumps.
		const addMeta = `CREATE TABLE IF NOT EXISTS meta (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
) WITHOUT ROWID`
		if _, err := db.Exec(addMeta); err != nil {
			return fmt.Errorf("sessionstate: migrate v5 (meta): %w", err)
		}
	}
	if from < 6 {
		// PLAN-286: a shared connection keys mutable state per logical agent, so
		// both persisted tables gain a logical_agent_id dimension. SQLite cannot
		// alter a WITHOUT-ROWID primary key, so each table is recreated with the
		// new column folded into its key; pre-existing rows back-fill to "" (the
		// connection-level agent), so an upgrade changes no behaviour until a
		// shared connection actually persists per-agent rows.
		const addReadAgent = `
CREATE TABLE read_tracking_v6 (
    proxy_session_id TEXT    NOT NULL,
    logical_agent_id TEXT    NOT NULL DEFAULT '',
    workspace        TEXT    NOT NULL,
    path             TEXT    NOT NULL,
    mtime_unix_nano  INTEGER NOT NULL,
    sha              TEXT    NOT NULL DEFAULT '',
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (proxy_session_id, logical_agent_id, workspace, path)
) WITHOUT ROWID;
INSERT INTO read_tracking_v6 (proxy_session_id, logical_agent_id, workspace, path, mtime_unix_nano, sha, updated_at)
    SELECT proxy_session_id, '', workspace, path, mtime_unix_nano, sha, updated_at FROM read_tracking;
DROP TABLE read_tracking;
ALTER TABLE read_tracking_v6 RENAME TO read_tracking;
CREATE INDEX IF NOT EXISTS idx_rt_updated ON read_tracking(updated_at);`
		if _, err := db.Exec(addReadAgent); err != nil {
			return fmt.Errorf("sessionstate: migrate v6 (read_tracking.logical_agent_id): %w", err)
		}
		const addPinAgent = `
CREATE TABLE pinned_workspace_v6 (
    proxy_session_id TEXT    NOT NULL,
    logical_agent_id TEXT    NOT NULL DEFAULT '',
    workspace        TEXT    NOT NULL,
    language         TEXT    NOT NULL DEFAULT '',
    source           TEXT    NOT NULL DEFAULT '',
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (proxy_session_id, logical_agent_id)
);
INSERT INTO pinned_workspace_v6 (proxy_session_id, logical_agent_id, workspace, language, source, updated_at)
    SELECT proxy_session_id, '', workspace, language, source, updated_at FROM pinned_workspace;
DROP TABLE pinned_workspace;
ALTER TABLE pinned_workspace_v6 RENAME TO pinned_workspace;`
		if _, err := db.Exec(addPinAgent); err != nil {
			return fmt.Errorf("sessionstate: migrate v6 (pinned_workspace.logical_agent_id): %w", err)
		}
	}
	if from < 7 {
		if err := migrateV7(db); err != nil {
			return err
		}
	}
	return nil
}

// migrateV7 turns session_names into the canonical durable identity record.
//
// Split out of migrate to keep that function under the complexity cap as the
// history grows; every step is still gated on the on-disk version by its one
// caller, so it runs exactly once per database.
func migrateV7(db *sql.DB) error {
	// PLAN-426: session_names becomes the CANONICAL durable identity record,
	// so it has to carry everything a recovery needs on its own.
	//
	// external_id is the authorised external-conversation linkage. It used to
	// live only in the predecessor's session JSON file, which is garbage
	// collected 24 h after the session ends — so an outage longer than that
	// silently dropped the linkage even though the identity itself survived.
	// A pre-v7 row back-fills to "", which means "unknown", never "none": the
	// JSON path still supplies it while that file is there, and a blank value
	// is never written over a known one (see SaveIdentity).
	//
	// name_revision orders name updates, so a proxy holding a snapshot taken
	// before an explicit rename cannot replay the older name over the newer
	// one. Pre-v7 rows start at 0 and take their first bump on the next save
	// that changes the name.
	//
	// Two ALTERs rather than a table rebuild: session_names has a rowid and a
	// single-column primary key, so ADD COLUMN with a default back-fills in
	// place — no copy, and no window in which the identity table is absent.
	const addExternal = `ALTER TABLE session_names ADD COLUMN external_id TEXT NOT NULL DEFAULT ''`
	if _, err := db.Exec(addExternal); err != nil {
		return fmt.Errorf("sessionstate: migrate v7 (session_names.external_id): %w", err)
	}
	const addRevision = `ALTER TABLE session_names ADD COLUMN name_revision INTEGER NOT NULL DEFAULT 0`
	if _, err := db.Exec(addRevision); err != nil {
		return fmt.Errorf("sessionstate: migrate v7 (session_names.name_revision): %w", err)
	}
	// The name index serves the reservation lookup, which now runs on every
	// name draw. It is deliberately NOT unique: legacy rows can already hold
	// the same name twice (before this release a name was only unique among
	// LIVE sessions, and a pruned row's name could be redrawn by another
	// proxy), and a unique index would fail the migration on exactly the
	// databases that most need it. LegacyNameConflicts reports those rows
	// instead of silently choosing an owner.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_sn_name ON session_names(name)`); err != nil {
		return fmt.Errorf("sessionstate: migrate v7 (idx_sn_name): %w", err)
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
// scoped by (proxySessionID, workspace) — the connection-level agent ("").
// nil-safe; a no-op when any key field is empty (an unidentified proxy or
// unpinned workspace cannot be rehydrated).
func (s *Store) UpsertRead(proxySessionID, workspace, path string, mtime time.Time, sha string) error {
	return s.UpsertReadForAgent(proxySessionID, "", workspace, path, mtime, sha)
}

// UpsertReadForAgent is UpsertRead scoped to one logical agent on a shared
// connection (PLAN-286): logicalAgentID "" is the connection-level agent.
func (s *Store) UpsertReadForAgent(proxySessionID, logicalAgentID, workspace, path string, mtime time.Time, sha string) error {
	if s == nil || proxySessionID == "" || workspace == "" || path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO read_tracking (proxy_session_id, logical_agent_id, workspace, path, mtime_unix_nano, sha, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(proxy_session_id, logical_agent_id, workspace, path)
		 DO UPDATE SET mtime_unix_nano=excluded.mtime_unix_nano, sha=excluded.sha, updated_at=excluded.updated_at`,
		proxySessionID, logicalAgentID, workspace, path, mtime.UnixNano(), sha, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("sessionstate: upsert read: %w", err)
	}
	return nil
}

// LoadReads returns every persisted read record for (proxySessionID, workspace)
// — the connection-level agent (""). nil-safe; returns nil when any key field is
// empty.
func (s *Store) LoadReads(proxySessionID, workspace string) ([]ReadRecord, error) {
	return s.LoadReadsForAgent(proxySessionID, "", workspace)
}

// LoadReadsForAgent is LoadReads scoped to one logical agent on a shared
// connection (PLAN-286): logicalAgentID "" is the connection-level agent.
func (s *Store) LoadReadsForAgent(proxySessionID, logicalAgentID, workspace string) ([]ReadRecord, error) {
	if s == nil || proxySessionID == "" || workspace == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT path, mtime_unix_nano, sha FROM read_tracking
		 WHERE proxy_session_id=? AND logical_agent_id=? AND workspace=?`,
		proxySessionID, logicalAgentID, workspace,
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
	return s.UpsertPinForAgent(proxySessionID, "", workspace, language, source)
}

// UpsertPinForAgent is UpsertPin scoped to one logical agent on a shared
// connection (PLAN-286): logicalAgentID "" is the connection-level agent.
func (s *Store) UpsertPinForAgent(proxySessionID, logicalAgentID, workspace, language string, source PinSource) error {
	if s == nil || proxySessionID == "" || workspace == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO pinned_workspace (proxy_session_id, logical_agent_id, workspace, language, source, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(proxy_session_id, logical_agent_id)
		 DO UPDATE SET workspace=excluded.workspace, language=excluded.language,
		               source=excluded.source, updated_at=excluded.updated_at`,
		proxySessionID, logicalAgentID, workspace, language, string(source), time.Now().UnixMilli(),
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
	return s.LoadPinForAgent(proxySessionID, "")
}

// LoadPinForAgent is LoadPin scoped to one logical agent on a shared connection
// (PLAN-286): logicalAgentID "" is the connection-level agent.
func (s *Store) LoadPinForAgent(proxySessionID, logicalAgentID string) (workspace, language string, source PinSource, ok bool, err error) {
	if s == nil || proxySessionID == "" {
		return "", "", PinSourceUnknown, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(
		`SELECT workspace, language, source FROM pinned_workspace WHERE proxy_session_id=? AND logical_agent_id=?`,
		proxySessionID, logicalAgentID,
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
	return s.DeletePinForAgent(proxySessionID, "")
}

// DeletePinForAgent is DeletePin scoped to one logical agent on a shared
// connection (PLAN-286): logicalAgentID "" is the connection-level agent.
func (s *Store) DeletePinForAgent(proxySessionID, logicalAgentID string) error {
	if s == nil || proxySessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM pinned_workspace WHERE proxy_session_id=? AND logical_agent_id=?`, proxySessionID, logicalAgentID); err != nil {
		return fmt.Errorf("sessionstate: delete pin: %w", err)
	}
	return nil
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

// Prune deletes EXPENDABLE persisted state last updated before olderThan,
// reclaiming rows left behind by a `plumb serve` that died without
// reconnecting. nil-safe.
//
// Expendable is the operative word, and session_names is deliberately NOT in
// it (PLAN-426). Read records and pins are caches: losing one costs a re-read
// or a re-pin. The identity row is the canonical proof of WHO a reconnecting
// serve is — the only thing that authorises it to resume its predecessor's ID,
// name and mailbox — so deleting it does not degrade a session, it forks one:
// the surviving proxy comes back as a stranger under a new ID and name, and
// mail addressed to the old one is orphaned. Age is no evidence that a serve
// process died; only the serve itself knows that, and it cannot say so once
// the daemon it would have told has restarted.
//
// The live exemption cannot stand in for this. It spares sessions connected to
// THIS daemon, and the sweep that matters runs at daemon startup, before any
// connection exists — so at the one moment a surviving serve most needs its
// row, the exemption list is empty. That is why retention is a property of the
// table, not of the caller's argument list.
//
// The cost is bounded and documented: one small row per proxy session, kept
// indefinitely, whose name stays reserved (see Reservations). Reclaiming one
// needs explicit retirement semantics — proof that the serve is gone, not a
// guess from elapsed time — which this deliberately does not invent.
//
// Rows belonging to a proxy session in live are kept regardless of age. Without
// that exemption the sweep reclaims state from sessions that are still
// connected: read rows are refreshed as the session works, but the pin is
// written once at initialize, so any conversation older than the TTL (24 h by
// default) loses it mid-flight and its next reconnect comes back unpinned.
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
	// session_names is intentionally absent — see the doc comment. Do not add a
	// DELETE here without an explicit retirement signal to gate it on.
	return nil
}
