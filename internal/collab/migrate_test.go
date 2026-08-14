package collab

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"sync"
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

// TestMigrate_V1NoteSurvivesAndClaimsAtMostOnce is the migration's real contract.
// collab.db is not a rebuildable index — its rows are the only copy — so a note
// written by the previous version must still be there afterwards, and must
// behave like an unread message: claimed at most once, then quiet.
func TestMigrate_V1NoteSurvivesAndClaimsAtMostOnce(t *testing.T) {
	ws := openV1WithNote(t, "written before threading existed", "alice")

	s, err := Open(ws)
	if err != nil {
		t.Fatalf("open (migrating) v1 store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	now := time.Now()

	got, err := s.ClaimNotesForSession(ctx, "alice", "sess-alice", "", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "written before threading existed" {
		t.Fatalf("legacy note lost in migration: %v", got)
	}
	if got[0].ConversationID != "" || got[0].OriginalBytes != 0 || got[0].TargetID != "" {
		t.Errorf("legacy defaults changed: conversation=%q original_bytes=%d target_id=%q",
			got[0].ConversationID, got[0].OriginalBytes, got[0].TargetID)
	}
	if got[0].DeliveredToID != "sess-alice" {
		t.Errorf("stable recipient identity was not stamped by the claim: %q", got[0].DeliveredToID)
	}
	again, err := s.ClaimNotes(ctx, "alice", "", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Error("a migrated legacy note must be claimed once, not on every read")
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
	cols, err := collabRowsColumns(db)
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

// TestMigrate_ConcurrentOpensOnV1 pins the cross-process migration race. SQLite
// has no ADD COLUMN IF NOT EXISTS, so the pragma_table_info check and the ALTER
// are two steps: when a second daemon (or a second connection pool, as here)
// opens the same v1 collab.db at the same moment, both see the column missing,
// both ALTER, and the loser used to fail its whole Open with "duplicate column
// name". A migration that another process already completed is success, not
// failure — and collab.db is not a rebuildable index, so a failed Open loses a
// project its only copy of the mailbox.
func TestMigrate_ConcurrentOpensOnV1(t *testing.T) {
	// Several fresh v1 databases, each raced independently. One round only
	// sometimes interleaves the pragma read with the ALTER, so a single round
	// makes this a ~90% pin; repeating on fresh files drives the chance that no
	// round ever races down to nothing, which is what makes it a regression test
	// rather than a coin flip.
	const rounds = 6
	for round := range rounds {
		t.Run(strconv.Itoa(round), migrateRaceRound)
	}
}

func migrateRaceRound(t *testing.T) {
	ws := openV1WithNote(t, "written before threading existed", "alice")

	const openers = 24
	var (
		mu       sync.Mutex
		failures []error
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for range openers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, err := Open(ws)
			if err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
				return
			}
			_ = s.Close()
		}()
	}
	close(start)
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d of %d concurrent opens failed (first: %v) — a migration a peer already "+
			"applied must not fail the opener that lost the race", len(failures), openers, failures[0])
	}

	// The migration must still be complete and additive: every chat column present
	// and the legacy row untouched.
	s, err := Open(ws)
	if err != nil {
		t.Fatalf("open after the concurrent burst: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cols, err := collabRowsColumns(s.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chatColumns {
		if !cols[c.name] {
			t.Errorf("column %s missing after concurrent migration", c.name)
		}
	}
	got, err := s.ClaimNotes(context.Background(), "alice", "", time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "written before threading existed" {
		t.Fatalf("legacy note lost to the concurrent migration: %v", got)
	}
}
