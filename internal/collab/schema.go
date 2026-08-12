package collab

import (
	"database/sql"
	"fmt"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

// schema.go holds collab.db's on-disk shape and the migration that brings an
// older file up to it. It is separate from the queries in store.go because the
// two answer different questions — what the table IS, versus what the mailbox
// DOES with it — and because the migration carries the constraint that shapes
// everything here: collab.db is NOT a rebuildable index. Its rows are the only
// copy of the data, so the schema may only ever grow, additively and in place.

const schema = `
CREATE TABLE IF NOT EXISTS collab_rows (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    kind           TEXT    NOT NULL,
    author_session TEXT    NOT NULL DEFAULT '',
    author_id      TEXT    NOT NULL DEFAULT '',
    body           TEXT    NOT NULL DEFAULT '',
    path_globs     TEXT    NOT NULL DEFAULT '',
    addressee      TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL DEFAULT 0,
    expires_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_collab_kind    ON collab_rows(kind);
CREATE INDEX IF NOT EXISTS idx_collab_expires ON collab_rows(expires_at);
CREATE INDEX IF NOT EXISTS idx_collab_author  ON collab_rows(author_id);
`

// chatColumns are every column added to collab_rows since the mailbox shipped:
// v2 turned a one-way note into a threaded, read-tracked message, and v3 added
// addressee_id, which binds a message to one particular session rather than to
// whoever happens to answer to its name.
//
// They are applied by ALTER TABLE rather than folded into the CREATE above so
// that an existing collab.db — which is NOT a rebuildable index; its rows are
// the only copy of the data — migrates in place instead of being dropped and
// recreated. Adding a column with a NOT NULL DEFAULT backfills every existing
// row, so an older note lands with an empty conversation, a zero delivered_at
// (i.e. unread), no origin workspace and no addressee id — exactly the values a
// note written before each of those existed should carry, and exactly what keeps
// it deliverable by name alone.
var chatColumns = []struct{ name, ddl string }{
	{"conversation_id", `ALTER TABLE collab_rows ADD COLUMN conversation_id TEXT NOT NULL DEFAULT ''`},
	{"delivered_at", `ALTER TABLE collab_rows ADD COLUMN delivered_at INTEGER NOT NULL DEFAULT 0`},
	{"delivered_to", `ALTER TABLE collab_rows ADD COLUMN delivered_to TEXT NOT NULL DEFAULT ''`},
	{"origin_workspace", `ALTER TABLE collab_rows ADD COLUMN origin_workspace TEXT NOT NULL DEFAULT ''`},
	{"target_workspace", `ALTER TABLE collab_rows ADD COLUMN target_workspace TEXT NOT NULL DEFAULT ''`},
	{"addressee_id", `ALTER TABLE collab_rows ADD COLUMN addressee_id TEXT NOT NULL DEFAULT ''`},
}

// chatIndexes accelerate the two hot chat queries: "what is unread for me" and
// "how many exchanges has this conversation had". Created after the columns
// exist, so they are separate from the base schema above.
const chatIndexes = `
CREATE INDEX IF NOT EXISTS idx_collab_inbox ON collab_rows(addressee, delivered_at, expires_at);
CREATE INDEX IF NOT EXISTS idx_collab_conv  ON collab_rows(conversation_id);
`

// schemaVersion is the current on-disk collab schema version, stamped in PRAGMA
// user_version. Unlike topology.db, collab.db is NOT a rebuildable index — its
// rows are the only copy of expiring advisory data — so a schema change must
// migrate additively rather than dropping the table. v1 was the initial shape;
// v2 added the chat columns and v3 addressee_id (see chatColumns), applied in
// place.
const schemaVersion = 3

func initDB(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("collab: apply schema: %w", err)
	}
	if err := migrateChatColumns(db); err != nil {
		return err
	}
	if _, err := db.Exec(chatIndexes); err != nil {
		return fmt.Errorf("collab: apply chat indexes: %w", err)
	}
	// Stamped unconditionally: it records what wrote the file. The migration above
	// is driven by which columns actually exist rather than by this stamp, so a
	// file written by a future plumb and reopened by an older one cannot be
	// mis-migrated on the strength of a version number alone.
	return sqlitex.StampVersion(db, schemaVersion)
}

// migrateChatColumns adds any chat column the table is missing. It is driven by
// PRAGMA table_info rather than the stamped version so it is idempotent and safe
// to run on every open, including on an older file that a crashed migration left
// half-way, and on one written by a plumb that knew fewer columns than this.
func migrateChatColumns(db *sql.DB) error {
	have, err := collabRowsColumns(db)
	if err != nil {
		return err
	}
	for _, c := range chatColumns {
		if have[c.name] {
			continue
		}
		if _, err := db.Exec(c.ddl); err != nil {
			// SQLite has no ADD COLUMN IF NOT EXISTS, so the inspection above and
			// this ALTER are two steps another process opening the same collab.db can
			// slip between — the loser gets "duplicate column name" and, without this,
			// its whole Open fails on work that has in fact been done. Re-inspect
			// rather than matching the driver's message text: if the column is present
			// now, a peer added it and there is nothing left to do; if it is still
			// absent, the failure is genuine and propagates.
			if after, qErr := collabRowsColumns(db); qErr == nil && after[c.name] {
				continue
			}
			return fmt.Errorf("collab: add column %s: %w", c.name, err)
		}
	}
	return nil
}

// collabRowsColumns returns the set of column names currently on collab_rows.
// It drives the migration, which must be decided by what the table actually has
// rather than by the stamped schema version.
func collabRowsColumns(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('collab_rows')`)
	if err != nil {
		return nil, fmt.Errorf("collab: inspect collab_rows: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("collab: inspect collab_rows: %w", err)
		}
		out[name] = true
	}
	return out, rows.Err()
}
