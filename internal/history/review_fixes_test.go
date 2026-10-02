package history

// Regression tests for the post-merge adversarial review of #580 (store core).

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

func indexNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_index_list('changes')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// Prune deletes changes with foreign keys on; without an index on the two
// referencing columns every deleted row scans the whole table (measured 34 s
// for 20k rows). A database written by an earlier build has no such indexes,
// so a read-write open must add them.
func TestReadWriteOpenAddsTheForeignKeyIndexes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.db")
	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Simulate a v1 file written before the indexes existed.
	db, err := sqlitex.Open(p, sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP INDEX IF EXISTS idx_ch_reverts`, `DROP INDEX IF EXISTS idx_ch_from`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err = Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	got := indexNames(t, s.db)
	for _, want := range []string{"idx_ch_reverts", "idx_ch_from"} {
		if !slices.Contains(got, want) {
			t.Errorf("index %s missing after a read-write open; have %v", want, got)
		}
	}
	if v, _ := sqlitex.Version(s.db); v != SchemaVersion {
		t.Errorf("user_version = %d; the indexes must not bump the schema version (older builds refuse newer files)", v)
	}
}

func TestPruneDeletesInBoundedChunks(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(t.TempDir(), "h.db")
	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		it := item(OpUpdate, filepath.Join(ws, "f"), []byte{'a' + byte(i), '\n'}, []byte{'b' + byte(i), '\n'})
		it.Workspace, it.At = ws, time.UnixMilli(int64(1000+i))
		s.Enqueue(it)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	db, err := sqlitex.Open(p, sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := prune(db, time.UnixMilli(5000), "", 2) // three chunks: 2 + 2 + 1
	if err != nil || res.Changes != 5 {
		t.Fatalf("chunked prune = %+v, %v; want all 5 changes", res, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM changes`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left = %d, %v", n, err)
	}
}

func metaValue(t *testing.T, db *sql.DB, key string) string {
	t.Helper()
	var v string
	err := db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return v
}

// When another writer holds the database lock for a whole batch, every row
// fails, the meta writes fail too, and the rows used to vanish without being
// counted anywhere. They must be counted, and the count must reach meta once
// the lock is free.
func TestALockedOutBatchIsCountedOnceTheLockClears(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.db")
	s, err := Open(p, Options{BusyTimeout: 20 * time.Millisecond, BatchWait: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())

	blocker, err := sqlitex.Open(p, sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := blocker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	s.Enqueue(item(OpUpdate, filepath.Join(t.TempDir(), "lost.txt"), []byte("a\n"), []byte("b\n")))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	blocker.Close()

	// The next batch, with the lock free, persists the carried-over counts.
	s.Enqueue(item(OpUpdate, filepath.Join(t.TempDir(), "ok.txt"), []byte("a\n"), []byte("b\n")))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := metaValue(t, s.db, "dropped_rows"); got == "" || got == "0" {
		t.Fatalf("dropped_rows = %q; the locked-out row was lost without being counted", got)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM changes`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("changes = %d, %v; want the second row (positive control: the writer recovered)", n, err)
	}
}

// A link remembered for a row that prune has since deleted must not turn the
// revert into a foreign-key failure and a dropped row.
func TestAStaleRevertLinkStillRecordsTheRevert(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(t.TempDir(), "h.db")
	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	f := filepath.Join(ws, "f")
	w := item(OpUpdate, f, []byte("a\n"), []byte("b\n"))
	w.Workspace, w.At = ws, time.UnixMilli(1000)
	s.Enqueue(w)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(p, time.UnixMilli(2000), ""); err != nil {
		t.Fatal(err)
	}
	rev := item(OpRevert, f, []byte("b\n"), []byte("a\n"))
	rev.Workspace, rev.At, rev.RevertsOwnCall = ws, time.UnixMilli(3000), true
	s.Enqueue(rev)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	var link sql.NullInt64
	var content string
	if err := s.db.QueryRow(`SELECT COUNT(*), MAX(reverts_seq), MAX(content) FROM changes WHERE op='revert'`).Scan(&n, &link, &content); err != nil {
		t.Fatal(err)
	}
	// Stored WITH its diff: a revert rescued only as a metadata marker (the
	// failed-insert retry) would hide the stale link instead of resolving it.
	if n != 1 || link.Valid || content != string(ContentDiff) {
		t.Fatalf("revert rows = %d, link = %+v, content = %q; want one revert with its diff and a NULL link", n, link, content)
	}
}

// An overflow marker that fails to insert is a dropped row, not an overflow row
// as well.
func TestAFailedOverflowMarkerIsCountedOnce(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.db")
	s, err := Open(p, Options{QueueSize: 1, BatchWait: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	s.pauseWriter()
	dir := t.TempDir()
	s.Enqueue(item(OpUpdate, filepath.Join(dir, "a"), []byte("1\n"), []byte("2\n")))
	s.Enqueue(item(OpUpdate, filepath.Join(dir, "b"), []byte("1\n"), []byte("2\n")))
	bad := item(OpUpdate, filepath.Join(dir, "c"), []byte("1\n"), []byte("2\n"))
	bad.Op = "bogus" // fails the op CHECK, as a row and as a marker
	s.Enqueue(bad)   // lands on the overflow list: an overflow marker
	s.resumeWriter()
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := metaValue(t, s.db, "dropped_rows"); got != "1" {
		t.Fatalf("dropped_rows = %q, want 1", got)
	}
	// overflow_rows must count exactly the markers that landed ("b" may have
	// overflowed too, legitimately); "c" never landed.
	var landed int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM changes WHERE content='withheld:overflow'`).Scan(&landed); err != nil {
		t.Fatal(err)
	}
	want := ""
	if landed > 0 {
		want = strconv.Itoa(landed)
	}
	if got := metaValue(t, s.db, "overflow_rows"); got != want {
		t.Fatalf("overflow_rows = %q but %d overflow markers are stored; a marker that never landed is not an overflow row", got, landed)
	}
}

// A rename moves content between two paths' chains: the rename row continues
// the SOURCE's chain (its before is the source's content), and it ends the
// source path's chain (the source no longer exists). Neither may read as an
// edit made outside plumb.
func TestRenamesDoNotFabricateGaps(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(t.TempDir(), "h.db")
	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(ws, "a.txt"), filepath.Join(ws, "b.txt")
	at := int64(1000)
	add := func(it Item) {
		at++
		it.Workspace, it.At = ws, time.UnixMilli(at)
		s.Enqueue(it)
	}
	add(item(OpCreate, a, nil, []byte("A\n")))
	add(item(OpCreate, b, nil, []byte("B\n")))
	// rename_file over an existing b.txt deletes it, then renames a.txt onto it.
	add(item(OpDelete, b, []byte("B\n"), nil))
	ren := item(OpRename, b, []byte("A\n"), []byte("A\n"))
	ren.From = a
	add(ren)
	// a.txt recreated, then b.txt edited after the rename.
	add(item(OpCreate, a, nil, []byte("C\n")))
	add(item(OpUpdate, b, []byte("A\n"), []byte("D\n")))
	add(item(OpUpdate, b, []byte("OUTSIDE\n"), []byte("E\n")))
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReadOnlyAt(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	es, err := r.List(Filter{Workspace: ws, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 7 {
		t.Fatalf("%d entries, want 7", len(es))
	}
	// Newest first: es[0] is the OUTSIDE edit — the positive control.
	if !es[0].GapBefore {
		t.Fatal("positive control: an edit whose before does not match the chain must show a gap")
	}
	for _, e := range es[1:] {
		if e.GapBefore {
			t.Errorf("false gap before seq %d (%s %s from %q)", e.Seq, e.Op, e.Path, e.From)
		}
	}
}
