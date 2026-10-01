package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneKeepsARevertWhoseAntecedentIsPruned(t *testing.T) {
	ws := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "h.db")
	s, _ := Open(dbPath, Options{})
	f := filepath.Join(ws, "f")
	w := item(OpUpdate, f, []byte("a\n"), []byte("b\n"))
	w.Workspace, w.At = ws, time.UnixMilli(1000)
	rev := item(OpRevert, f, []byte("b\n"), []byte("a\n"))
	rev.Workspace, rev.At, rev.RevertsOwnCall = ws, time.UnixMilli(5000), true
	cp := item(OpCopy, filepath.Join(ws, "g"), nil, []byte("x\n"))
	cp.Workspace, cp.At, cp.From = ws, time.UnixMilli(6000), filepath.Join(ws, "src")
	s.Enqueue(w)
	s.Enqueue(rev)
	s.Enqueue(cp)
	_ = s.Close(context.Background())

	res, err := Prune(dbPath, time.UnixMilli(2000), "")
	if err != nil || res.Changes != 1 {
		t.Fatalf("Prune = %+v, %v (FK must not abort: reverts_seq is ON DELETE SET NULL)", res, err)
	}
	r, _ := OpenReadOnlyAt(dbPath)
	defer r.Close()
	es, _ := r.List(Filter{All: true, Limit: 10})
	if len(es) != 2 || es[1].RevertsSeq != 0 {
		t.Fatalf("after prune = %+v; revert kept with a NULL link", es)
	}
	if es[0].From != "src" {
		t.Fatalf("copy source path was garbage-collected: %+v", es[0])
	}
}
