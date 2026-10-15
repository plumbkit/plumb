package collab

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

// namespace_store_test.go pins the store-level half of namespaces: what the view
// type promises (a shared handle, a no-op Close), the one statement that crosses
// namespaces on purpose (PurgeNamespace), and the v4 → v5 migration that gives
// every pre-existing row to the default namespace.

// TestNamespace_PurgeRemovesExactlyOneNamespace: three namespaces each hold an
// intent and a note; purging one leaves the others' rows untouched, and does so
// whichever view it is called through.
func TestNamespace_PurgeRemovesExactlyOneNamespace(t *testing.T) {
	base, _ := openTestStore(t)
	views := nsViews(base)
	ctx, now := context.Background(), time.Now()
	for _, ns := range nsAll {
		if err := views[ns].PutIntent(ctx, IntentInput{AuthorSession: "carol", AuthorID: "id-carol", Body: "i", TTL: time.Hour}, now); err != nil {
			t.Fatal(err)
		}
		mustPut(t, views[ns], NoteInput{AuthorID: "id-bob", Body: "n", Addressee: "alice"}, now)
		mustPut(t, views[ns], NoteInput{AuthorID: "id-bob", Body: "n2", Addressee: "alice"}, now)
	}
	for _, ns := range nsAll {
		if got := rowsIn(t, base, ns); got != 3 {
			t.Fatalf("setup: namespace %q holds %d rows, want 3", ns, got)
		}
	}

	// Called through ns-b's view, it still purges the namespace it is NAMED.
	n, err := views["ns-b"].PurgeNamespace(ctx, "ns-a")
	if err != nil || n != 3 {
		t.Fatalf("PurgeNamespace(ns-a) = %d, %v; want 3 rows removed", n, err)
	}
	for ns, want := range map[string]int{"": 3, "ns-a": 0, "ns-b": 3} {
		if got := rowsIn(t, base, ns); got != want {
			t.Errorf("after purging ns-a, namespace %q holds %d rows, want %d", ns, got, want)
		}
	}

	if n, err := base.PurgeNamespace(ctx, "ns-a"); err != nil || n != 0 {
		t.Errorf("purging an empty namespace = %d, %v; want 0, nil", n, err)
	}
	if n, err := base.PurgeNamespace(ctx, "never-existed"); err != nil || n != 0 {
		t.Errorf("purging an unknown namespace = %d, %v; want 0, nil", n, err)
	}
	// The default namespace's mail is still deliverable after another's purge.
	got, err := base.ClaimNotes(ctx, Claimant{Name: "alice", ID: "id-alice"}, now, 0)
	if err != nil || len(got) != 2 {
		t.Errorf("default claim after a purge = %v (err %v), want its 2 notes", bodies(got), err)
	}
}

// TestNamespace_PurgeRefusesTheDefaultNamespace: "" is every row that predates
// namespaces and every session that never chose one. An empty name reaching the
// delete must be an error, with nothing removed.
func TestNamespace_PurgeRefusesTheDefaultNamespace(t *testing.T) {
	base, _ := openTestStore(t)
	ctx, now := context.Background(), time.Now()
	mustPut(t, base, NoteInput{AuthorID: "id-bob", Body: "default mail", Addressee: "alice"}, now)
	mustPut(t, base.InNamespace("ns-a"), NoteInput{AuthorID: "id-bob", Body: "other mail", Addressee: "alice"}, now)

	for name, s := range map[string]*Store{"default": base, "view": base.InNamespace("ns-a"), "nil": nil} {
		if n, err := s.PurgeNamespace(ctx, ""); err == nil || n != 0 {
			t.Errorf("%s PurgeNamespace(\"\") = %d, %v; want a refusal and 0 rows", name, n, err)
		}
	}
	for ns, want := range map[string]int{"": 1, "ns-a": 1} {
		if got := rowsIn(t, base, ns); got != want {
			t.Errorf("after the refused purges, namespace %q holds %d rows, want %d", ns, got, want)
		}
	}
	if n, err := (*Store)(nil).PurgeNamespace(ctx, "ns-a"); err != nil || n != 0 {
		t.Errorf("nil store PurgeNamespace(ns-a) = %d, %v; want 0, nil as Prune does", n, err)
	}
}

// TestNamespace_PruneStaysNamespaceAgnostic: the expiry sweep reclaims every
// namespace's dead rows in one pass and touches no live one.
func TestNamespace_PruneStaysNamespaceAgnostic(t *testing.T) {
	base, _ := openTestStore(t)
	views := nsViews(base)
	ctx, now := context.Background(), time.Now()
	for _, ns := range nsAll {
		mustPut(t, views[ns], NoteInput{AuthorID: "id-bob", Body: "short", Addressee: "alice", TTL: time.Minute}, now)
		mustPut(t, views[ns], NoteInput{AuthorID: "id-bob", Body: "long", Addressee: "alice", TTL: time.Hour}, now)
	}
	n, err := base.Prune(ctx, now.Add(30*time.Minute))
	if err != nil || n != len(nsAll) {
		t.Fatalf("Prune = %d, %v; want one expired row from each of %d namespaces", n, err, len(nsAll))
	}
	for _, ns := range nsAll {
		if got := rowsIn(t, base, ns); got != 1 {
			t.Errorf("namespace %q holds %d rows after the sweep, want its 1 live row", ns, got)
		}
	}
}

// TestNamespace_ViewCloseIsANoOp: a view shares the handle, so closing it must not
// close the store every other namespace is still using.
func TestNamespace_ViewCloseIsANoOp(t *testing.T) {
	ws := t.TempDir()
	base, err := Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	a := base.InNamespace("ns-a")
	ctx, now := context.Background(), time.Now()

	if err := a.Close(); err != nil {
		t.Fatalf("view Close = %v, want nil", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second view Close = %v, want nil", err)
	}
	// Both the base and the (closed) view still work: the handle is open.
	if err := base.PutIntent(ctx, IntentInput{AuthorSession: "c", AuthorID: "id-c", Body: "base still works", TTL: time.Hour}, now); err != nil {
		t.Fatalf("base store after a view's Close: %v", err)
	}
	if err := a.PutIntent(ctx, IntentInput{AuthorSession: "c", AuthorID: "id-c", Body: "view still works", TTL: time.Hour}, now); err != nil {
		t.Fatalf("view after its own Close: %v", err)
	}

	// Positive control: closing the owning store really closes the handle.
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}
	if err := base.db.PingContext(ctx); err == nil {
		t.Error("the owning store's Close left the handle open")
	}
}

// TestNamespace_ViewAccessors: Namespace reports what the view is scoped to, a view
// of a view is a view of the original store in the NEW namespace (not an
// intersection), and a nil store stays nil-safe.
func TestNamespace_ViewAccessors(t *testing.T) {
	base, _ := openTestStore(t)
	if base.Namespace() != "" {
		t.Errorf("Open's store Namespace = %q, want the default", base.Namespace())
	}
	a := base.InNamespace("ns-a")
	if a.Namespace() != "ns-a" {
		t.Errorf("view Namespace = %q, want ns-a", a.Namespace())
	}
	b := a.InNamespace("ns-b")
	if b.Namespace() != "ns-b" || b.db != base.db {
		t.Errorf("view of a view: Namespace %q, shared handle %v; want ns-b over the same handle", b.Namespace(), b.db == base.db)
	}
	back := a.InNamespace("")
	if back.Namespace() != "" || !back.view {
		t.Errorf("InNamespace(\"\") = namespace %q view %v; want a default-namespace VIEW, whose Close is still a no-op", back.Namespace(), back.view)
	}

	var none *Store
	if none.InNamespace("ns-a") != nil {
		t.Error("InNamespace on a nil store must be nil")
	}
	if none.Namespace() != "" {
		t.Error("Namespace on a nil store must be the default")
	}
	if err := none.InNamespace("ns-a").Close(); err != nil {
		t.Errorf("Close on a nil view = %v, want nil", err)
	}
}

// v4Columns is every column collab.db had at schema v4, reproduced verbatim so the
// fixture stays frozen at the historical shape: deriving it from chatColumns would
// make it grow with every future column and stop testing the upgrade it is named for.
var v4Columns = []string{
	`ALTER TABLE collab_rows ADD COLUMN conversation_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE collab_rows ADD COLUMN delivered_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE collab_rows ADD COLUMN delivered_to TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE collab_rows ADD COLUMN origin_workspace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE collab_rows ADD COLUMN target_workspace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE collab_rows ADD COLUMN addressee_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE collab_rows ADD COLUMN delivered_to_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE collab_rows ADD COLUMN origin_git_common_dir TEXT NOT NULL DEFAULT ''`,
}

// openV4WithRows writes a collab.db in the exact shape schema v4 left: no namespace
// column, one unread note to alice and one live intent.
func openV4WithRows(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	db, err := sqlitex.Open(DBPath(ws), sqlitex.Options{})
	if err != nil {
		t.Fatalf("open v4 db: %v", err)
	}
	if _, err := db.Exec(v1Schema); err != nil {
		t.Fatalf("apply base schema: %v", err)
	}
	for _, ddl := range v4Columns {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("apply v4 column: %v", err)
		}
	}
	now := time.Now()
	for _, row := range []struct{ kind, author, body, addressee string }{
		{string(KindNote), "id-bob", "written under v4", "alice"},
		{string(KindIntent), "id-carol", "intent under v4", ""},
	} {
		if _, err := db.Exec(
			`INSERT INTO collab_rows (kind, author_session, author_id, body, path_globs, addressee, created_at, expires_at, conversation_id)
			 VALUES (?, ?, ?, ?, '', ?, ?, ?, 'cv4')`,
			row.kind, row.author, row.author, row.body, row.addressee, now.UnixNano(), now.Add(time.Hour).UnixNano()); err != nil {
			t.Fatalf("insert v4 row: %v", err)
		}
	}
	if err := sqlitex.StampVersion(db, 4); err != nil {
		t.Fatalf("stamp v4: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return ws
}

// TestMigrate_V4RowsBackfillTheDefaultNamespace: collab.db is not a rebuildable
// index, so a v4 file must open in place, keep both rows, put them in the default
// namespace, and leave them readable by the default store — and by no other.
func TestMigrate_V4RowsBackfillTheDefaultNamespace(t *testing.T) {
	ws := openV4WithRows(t)
	s, err := Open(ws)
	if err != nil {
		t.Fatalf("open (migrating) v4 store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, now := context.Background(), time.Now()

	var distinct, nonDefault int
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT namespace), SUM(namespace != '') FROM collab_rows`).Scan(&distinct, &nonDefault); err != nil {
		t.Fatal(err)
	}
	if distinct != 1 || nonDefault != 0 || rowsIn(t, s, "") != 2 {
		t.Fatalf("backfill: %d distinct namespaces, %d non-default, %d default rows; want every row in '' and none lost",
			distinct, nonDefault, rowsIn(t, s, ""))
	}

	live, err := s.LiveIntents(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	wantBodies(t, "default LiveIntents on a migrated file", live, "intent under v4")

	other := s.InNamespace("ns-a")
	if got := countsFor(t, other, Claimant{Name: "alice", ID: "id-alice"}, now); got != (readerCounts{}) {
		t.Errorf("a named namespace sees the migrated default mail: %+v", got)
	}
	if live, err := other.LiveIntents(ctx, now); err != nil || len(live) != 0 {
		t.Errorf("a named namespace sees the migrated default intents: %v (err %v)", bodies(live), err)
	}

	got, err := s.ClaimNotes(ctx, Claimant{Name: "alice", ID: "id-alice"}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantBodies(t, "default ClaimNotes on a migrated file", got, "written under v4")
	if got[0].ConversationID != "cv4" {
		t.Errorf("ConversationID = %q, want the v4 thread preserved", got[0].ConversationID)
	}

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 5 {
		t.Errorf("user_version = %d (err %v), want 5", version, err)
	}
}

// TestMigrate_V4ToV5IsConcurrentSafe: several connections opening the same v4 file
// at once all see namespace missing and all try to add it; the losers must treat
// "a peer already did it" as success, as the earlier columns' race test pins.
func TestMigrate_V4ToV5IsConcurrentSafe(t *testing.T) {
	for range 4 {
		raceOpens(t, openV4WithRows(t), "written under v4")
	}
}

// TestMigrate_NamespaceColumnIsAdditive: a v5 file reopened repeatedly keeps its
// namespaced rows, and an older binary's insert (no namespace column in the
// statement) lands in the default namespace by the column's DEFAULT.
func TestMigrate_NamespaceColumnIsAdditive(t *testing.T) {
	ws := t.TempDir()
	s, err := Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	ctx, now := context.Background(), time.Now()
	mustPut(t, s.InNamespace("ns-a"), NoteInput{AuthorID: "id-bob", Body: "kept", Addressee: "alice"}, now)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		s, err = Open(ws)
		if err != nil {
			t.Fatal(err)
		}
		if got := rowsIn(t, s, "ns-a"); got != 1 {
			t.Fatalf("namespaced rows after reopen = %d, want 1", got)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	db, err := sql.Open("sqlite", filepath.ToSlash(DBPath(ws)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO collab_rows (kind, author_session, author_id, body, path_globs, addressee, created_at, expires_at)
		 VALUES ('note', 'old', 'id-old', 'from an older plumb', '', 'alice', ?, ?)`,
		now.UnixNano(), now.Add(time.Hour).UnixNano()); err != nil {
		t.Fatalf("an older binary's insert must still work: %v", err)
	}
	var ns string
	if err := db.QueryRowContext(ctx, `SELECT namespace FROM collab_rows WHERE body = 'from an older plumb'`).Scan(&ns); err != nil || ns != "" {
		t.Errorf("an older binary's row has namespace %q (err %v), want the default ''", ns, err)
	}
}
