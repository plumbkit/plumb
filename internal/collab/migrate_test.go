package collab

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

// v1Schema is the schema exactly as plumb shipped it before threading and read
// tracking existed. Reproduced verbatim so the migration is tested against the
// real historical shape rather than against a guess at it.
const v1Schema = `
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
);`

// openV1WithNote writes a v1 collab.db holding one addressed note, and returns
// the workspace it lives in.
func openV1WithNote(t *testing.T, body, addressee string) string {
	t.Helper()
	ws := t.TempDir()
	db, err := sqlitex.Open(DBPath(ws), sqlitex.Options{})
	if err != nil {
		t.Fatalf("open v1 db: %v", err)
	}
	if _, err := db.Exec(v1Schema); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	now := time.Now()
	if _, err := db.Exec(
		`INSERT INTO collab_rows (kind, author_session, author_id, body, path_globs, addressee, created_at, expires_at)
		 VALUES (?, 'legacy-author', 'legacy-id', ?, '', ?, ?, ?)`,
		string(KindNote), body, addressee, now.UnixNano(), now.Add(time.Hour).UnixNano()); err != nil {
		t.Fatalf("insert legacy note: %v", err)
	}
	if err := sqlitex.StampVersion(db, 1); err != nil {
		t.Fatalf("stamp v1: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v1 db: %v", err)
	}
	return ws
}

// TestMigrate_V1NoteSurvivesAndDeliversOnce is the migration's real contract.
// collab.db is not a rebuildable index — its rows are the only copy — so a note
// written by the previous version must still be there afterwards, and must
// behave like an unread message: delivered exactly once, then quiet.
func TestMigrate_V1NoteSurvivesAndDeliversOnce(t *testing.T) {
	ws := openV1WithNote(t, "written before threading existed", "alice")

	s, err := Open(ws)
	if err != nil {
		t.Fatalf("open (migrating) v1 store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	now := time.Now()

	got, err := s.ClaimNotes(ctx, "alice", "", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "written before threading existed" {
		t.Fatalf("legacy note lost in migration: %v", got)
	}
	if got[0].ConversationID != "" {
		t.Errorf("a legacy row has no conversation; got %q", got[0].ConversationID)
	}
	again, err := s.ClaimNotes(ctx, "alice", "", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Error("a migrated legacy note must be delivered once, not on every read")
	}
}

// TestMigrate_IsIdempotent: the migration is driven by which columns exist, not
// by the stamped version, so re-opening an already-migrated file is a no-op and
// a half-applied migration can be completed on the next open.
func TestMigrate_IsIdempotent(t *testing.T) {
	ws := openV1WithNote(t, "note", "alice")

	for i := range 3 {
		s, err := Open(ws)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close #%d: %v", i, err)
		}
	}

	db, err := sql.Open("sqlite", filepath.ToSlash(DBPath(ws)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cols, err := tableColumns(db, "collab_rows")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chatColumns {
		if !cols[c.name] {
			t.Errorf("column %s missing after migration", c.name)
		}
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
}
