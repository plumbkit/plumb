package sessionstate

// migrate.go — the schema history.
//
// Split from db.go by responsibility: db.go owns the live queries, this file
// owns how a database on disk reaches the shape those queries assume. Keeping
// them together pushed db.go past the file-size cap as the history grew, which
// is the same pressure that pushed migrate itself past the complexity cap.

import (
	"database/sql"
	"fmt"
)

// migrationSteps is the ordered schema history: index N holds the step that
// brings a database to version N. A nil entry is a version that needed no
// change.
//
// A table rather than a chain of `if from < N` blocks, because that chain cost
// two branches per version and had already pushed migrate over the complexity
// cap twice — a tax the history pays again at every future version, for a
// function whose logic never actually changes. The loop is now constant.
var migrationSteps = map[int]func(*sql.Tx) error{
	2: migrateV2,
	3: migrateV3,
	4: migrateV4,
	5: migrateV5,
	6: migrateV6,
	7: migrateV7,
	8: migrateV8,
}

// runMigrationStep applies one step and advances user_version to that step's
// version IN THE SAME TRANSACTION.
//
// Before this, steps ran bare and the version was stamped only once every step
// had finished. A crash or an error midway left the schema partly migrated with
// user_version still at the old value, so the next open replayed steps that had
// already run — and they are not idempotent: `ALTER TABLE ... ADD COLUMN` fails
// with "duplicate column name", and the state database could not be opened again
// without manual repair. SQLite makes DDL transactional, so a step now either
// applies and advances, or does neither, and an interrupted upgrade simply
// resumes from the last step that completed.
//
// user_version cannot be parameterised — SQLite takes a PRAGMA value as a
// literal — so the version is formatted in. It is an int from this package's own
// step table, never caller input.
func runMigrationStep(db *sql.DB, version int, step func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("sessionstate: begin migration v%d: %w", version, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := step(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("sessionstate: stamp migration v%d: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sessionstate: commit migration v%d: %w", version, err)
	}
	return nil
}

// migrate brings a database at version `from` up to SchemaVersion. Each step is
// gated on the on-disk version, so it runs exactly once per database and a
// re-open is a no-op. The baseline `schema` above is frozen at v1, so a fresh
// database (version 0) and an upgraded one converge on the same shape here.
func migrate(db *sql.DB, from int) error {
	for v := from + 1; v <= SchemaVersion; v++ {
		step, ok := migrationSteps[v]
		if !ok {
			continue
		}
		if err := runMigrationStep(db, v, step); err != nil {
			return err
		}
	}
	return nil
}

// migrateV2 is schema step v2. Extracted so migrate stays a dispatch loop;
// the narrative for this step lives with the statements it runs.
func migrateV2(tx *sql.Tx) error {
	// SQLite permits a NOT NULL column via ADD COLUMN when it carries a
	// default, which back-fills every pre-existing row to the unknown origin.
	const addSource = `ALTER TABLE pinned_workspace ADD COLUMN source TEXT NOT NULL DEFAULT ''`
	if _, err := tx.Exec(addSource); err != nil {
		return fmt.Errorf("sessionstate: migrate v2 (pinned_workspace.source): %w", err)
	}
	return nil
}

// migrateV3 is schema step v3. Extracted so migrate stays a dispatch loop;
// the narrative for this step lives with the statements it runs.
func migrateV3(tx *sql.Tx) error {
	// A dedicated table, not a column on pinned_workspace: a pin row only
	// exists once a workspace is pinned, but the name must be recorded for
	// every identified proxy session, pinned or not.
	const addNames = `CREATE TABLE IF NOT EXISTS session_names (
    proxy_session_id TEXT    PRIMARY KEY,
    name             TEXT    NOT NULL,
    updated_at       INTEGER NOT NULL
)`
	if _, err := tx.Exec(addNames); err != nil {
		return fmt.Errorf("sessionstate: migrate v3 (session_names): %w", err)
	}
	return nil
}

// migrateV4 is schema step v4. Extracted so migrate stays a dispatch loop;
// the narrative for this step lives with the statements it runs.
func migrateV4(tx *sql.Tx) error {
	// The plumb session ID the name belonged to, so a reconnecting proxy can
	// inherit its predecessor's mailbox identity and collect messages bound to
	// it. A pre-v4 row back-fills to "" and inherits nothing, which is the
	// behaviour that shipped before this column existed.
	const addSessionID = `ALTER TABLE session_names ADD COLUMN plumb_session_id TEXT NOT NULL DEFAULT ''`
	if _, err := tx.Exec(addSessionID); err != nil {
		return fmt.Errorf("sessionstate: migrate v4 (session_names.plumb_session_id): %w", err)
	}
	return nil
}

// migrateV5 is schema step v5. Extracted so migrate stays a dispatch loop;
// the narrative for this step lives with the statements it runs.
func migrateV5(tx *sql.Tx) error {
	// A key/value side table rather than more columns: these are facts about
	// the DATABASE (which one-shot maintenance has already run), not about any
	// proxy session. The schema version is the wrong place to record "has this
	// run yet", because that answer has to survive later version bumps.
	const addMeta = `CREATE TABLE IF NOT EXISTS meta (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
) WITHOUT ROWID`
	if _, err := tx.Exec(addMeta); err != nil {
		return fmt.Errorf("sessionstate: migrate v5 (meta): %w", err)
	}
	return nil
}

// migrateV6 is schema step v6. Extracted so migrate stays a dispatch loop;
// the narrative for this step lives with the statements it runs.
func migrateV6(tx *sql.Tx) error {
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
	if _, err := tx.Exec(addReadAgent); err != nil {
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
	if _, err := tx.Exec(addPinAgent); err != nil {
		return fmt.Errorf("sessionstate: migrate v6 (pinned_workspace.logical_agent_id): %w", err)
	}
	return nil
}

// migrateV7 turns session_names into the canonical durable identity record.
//
// Split out of migrate to keep that function under the complexity cap as the
// history grows; every step is still gated on the on-disk version by its one
// caller, so it runs exactly once per database.
func migrateV7(tx *sql.Tx) error {
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
	if _, err := tx.Exec(addExternal); err != nil {
		return fmt.Errorf("sessionstate: migrate v7 (session_names.external_id): %w", err)
	}
	const addRevision = `ALTER TABLE session_names ADD COLUMN name_revision INTEGER NOT NULL DEFAULT 0`
	if _, err := tx.Exec(addRevision); err != nil {
		return fmt.Errorf("sessionstate: migrate v7 (session_names.name_revision): %w", err)
	}
	// The name index serves the reservation lookup, which now runs on every
	// name draw. It is deliberately NOT unique, and that is not only about
	// history: a name was once unique only among LIVE sessions, and a serve
	// RESTART minted a second row for a name the first still held, so duplicates
	// could be written at any time. A unique index would fail the migration on
	// exactly the databases that most need it, and a name held by two different
	// conversations is a standing ambiguity rather than a thing to be resolved on
	// migration. Every one of those rows is legitimate history — the durable proof
	// of which session a reconnecting proxy is — so a conversation's own
	// superseded rows are collapsed when the name is READ (see independentClaims)
	// and never deleted.
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_sn_name ON session_names(name)`); err != nil {
		return fmt.Errorf("sessionstate: migrate v7 (idx_sn_name): %w", err)
	}
	return nil
}

// migrateV8 adds the durable record of which logical agents were multiplexed
// over a connection.
//
// Split out of migrate for the same reason migrateV7 was: to keep that function
// under the complexity cap as the history grows. Gated on the on-disk version by
// its one caller, so it runs exactly once per database.
func migrateV8(tx *sql.Tx) error {
	// PLAN-440 item 2: the durable record of WHICH logical agents were
	// multiplexed over a connection, independent of whether any of them
	// pinned a workspace.
	//
	// pinned_workspace was the obvious source and the wrong one: a row is
	// written there only when an agent's own session_start named a
	// workspace, while an agent identifies itself through three channels —
	// an attach-time session_id, a per-call _meta stamp, and session_start's
	// own argument. A subagent that stamps its calls and inherits the
	// connection's pin, which is the common topology, never wrote a row, so
	// a reconnecting daemon read a genuinely shared connection as
	// single-agent and left the fail-closed ceiling disarmed.
	const addAgents = `CREATE TABLE IF NOT EXISTS logical_agent (
    proxy_session_id TEXT    NOT NULL,
    logical_agent_id TEXT    NOT NULL,
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (proxy_session_id, logical_agent_id)
) WITHOUT ROWID`
	if _, err := tx.Exec(addAgents); err != nil {
		return fmt.Errorf("sessionstate: migrate v8 (logical_agent): %w", err)
	}

	return nil
}
