package history

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/plumbkit/plumb/internal/textdiff"
)

func openTestStore(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "history.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func item(op Op, path string, before, after []byte) Item {
	it := Item{Workspace: filepath.Dir(path), CallID: "C1", SessionID: "S", Change: Change{
		At: time.UnixMilli(1_790_000_000_123), Op: op, Kind: KindFile, Tool: "write_file", Path: path,
	}}
	if before != nil {
		it.Before = SideFromBytes(before)
	}
	if after != nil {
		it.After = SideFromBytes(after)
	}
	return it
}

type rowT struct {
	seq, ts                    int64
	op, content, path, callID  string
	added, removed, redactions int
	diff                       []byte
	reverts                    sql.NullInt64
}

func allRows(t *testing.T, s *Store) []rowT {
	t.Helper()
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT c.seq, c.ts_ms, c.op, c.content, p.path, c.call_id, c.added, c.removed, c.redactions, c.diff, c.reverts_seq
		FROM changes c JOIN paths p ON p.id = c.path_id ORDER BY c.ts_ms, c.seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []rowT
	for rows.Next() {
		var r rowT
		if err := rows.Scan(&r.seq, &r.ts, &r.op, &r.content, &r.path, &r.callID, &r.added, &r.removed, &r.redactions, &r.diff, &r.reverts); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestWriterStoresAnExactCompressedDiff(t *testing.T) {
	s := openTestStore(t, Options{})
	before, after := []byte("a\nb\nc\n"), []byte("a\nB\nc")
	dir := t.TempDir()
	s.Enqueue(item(OpUpdate, filepath.Join(dir, "f.txt"), before, after))
	r := allRows(t, s)
	if len(r) != 1 || r[0].content != string(ContentDiff) || r[0].added != 2 || r[0].removed != 2 || r[0].ts != 1_790_000_000_123 {
		t.Fatalf("row = %+v", r)
	}
	dec, _ := zstd.NewReader(nil)
	plain, err := dec.DecodeAll(r[0].diff, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := textdiff.Apply(string(before), string(plain))
	if err != nil || got != string(after) {
		t.Fatalf("stored diff does not reapply: %q, %v", got, err)
	}
	if r[0].path != "f.txt" {
		t.Fatalf("path stored as %q; want workspace-relative", r[0].path)
	}
}

func TestWriterWithholdsBinary(t *testing.T) {
	s := openTestStore(t, Options{})
	s.Enqueue(item(OpCreate, filepath.Join(t.TempDir(), "img.png"), nil, []byte("\x89PNG\x00\x01")))
	if r := allRows(t, s); r[0].content != string(ContentBinary) || r[0].diff != nil {
		t.Fatalf("binary row = %+v", r[0])
	}
}

func TestWriterNoOpWriteRecordsNone(t *testing.T) {
	s := openTestStore(t, Options{})
	s.Enqueue(item(OpUpdate, filepath.Join(t.TempDir(), "same.txt"), []byte("x\n"), []byte("x\n")))
	if r := allRows(t, s); r[0].content != string(ContentNone) || r[0].added+r[0].removed != 0 {
		t.Fatalf("no-op row = %+v", r[0])
	}
}

func TestWriterCountsBeforeRedaction(t *testing.T) {
	s := openTestStore(t, Options{})
	secret := "token = ghp_" + string(bytes.Repeat([]byte("a"), 36)) + "\n"
	s.Enqueue(item(OpCreate, filepath.Join(t.TempDir(), "conf.txt"), nil, []byte(secret)))
	r := allRows(t, s)
	if r[0].redactions == 0 || r[0].added != 1 {
		t.Fatalf("redacted row = %+v (want redactions>0, added counted from the raw script)", r[0])
	}
}

func TestWriterDiffOverMaxIsWithheld(t *testing.T) {
	s := openTestStore(t, Options{MaxDiffBytes: func() int64 { return 10 }})
	s.Enqueue(item(OpCreate, filepath.Join(t.TempDir(), "f"), nil, []byte("0123456789abcdef\n")))
	if r := allRows(t, s); r[0].content != string(ContentTooLarge) {
		t.Fatalf("row = %+v", r[0])
	}
}

func TestRevertLinksToTheCallsLatestRowForThePath(t *testing.T) {
	s := openTestStore(t, Options{})
	p := filepath.Join(t.TempDir(), "f.txt")
	first := item(OpUpdate, p, []byte("1\n"), []byte("2\n"))
	second := item(OpUpdate, p, []byte("2\n"), []byte("3\n"))
	rev := item(OpRevert, p, []byte("3\n"), []byte("1\n"))
	rev.RevertsOwnCall, rev.Reason = true, "tx_rollback"
	// Enqueue all three before the writer runs: the antecedent has no seq yet
	// when the revert is queued (review vast-stream #3).
	s.Enqueue(first)
	s.Enqueue(second)
	s.Enqueue(rev)
	r := allRows(t, s)
	if !r[2].reverts.Valid || r[2].reverts.Int64 != r[1].seq {
		t.Fatalf("revert links %+v; want seq %d (latest row of the call for this path)", r[2].reverts, r[1].seq)
	}
}

func TestUndoRevertLinksByWrittenSHA(t *testing.T) {
	s := openTestStore(t, Options{})
	p := filepath.Join(t.TempDir(), "f.txt")
	w := item(OpUpdate, p, []byte("a\n"), []byte("b\n"))
	s.Enqueue(w)
	undo := item(OpRevert, p, []byte("DIVERGED\n"), []byte("a\n")) // force:true on a diverged file
	undo.CallID, undo.RevertsSHA, undo.Reason = "C2", w.After.SHA(), "undo_edit"
	s.Enqueue(undo)
	r := allRows(t, s)
	if !r[1].reverts.Valid || r[1].reverts.Int64 != r[0].seq {
		t.Fatalf("undo revert = %+v; want link to %d", r[1].reverts, r[0].seq)
	}
	orphan := item(OpRevert, p, []byte("x\n"), []byte("y\n"))
	orphan.CallID, orphan.RevertsSHA = "C3", []byte("no-such-sha")
	s.Enqueue(orphan)
	if r := allRows(t, s); r[2].reverts.Valid {
		t.Fatal("unmatched undo must leave reverts_seq NULL")
	}
}
