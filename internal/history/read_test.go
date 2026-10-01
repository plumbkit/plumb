package history

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

func seedStore(t *testing.T) (dbPath, ws string) {
	t.Helper()
	ws = t.TempDir()
	dbPath = filepath.Join(t.TempDir(), "history.db")
	s, err := Open(dbPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(ws, "f.txt")
	a := item(OpUpdate, f, []byte("1\n"), []byte("2\n"))
	a.Workspace, a.At = ws, time.UnixMilli(1000)
	b := item(OpUpdate, f, []byte("EXTERNAL\n"), []byte("3\n")) // before ≠ a.after → a gap
	b.Workspace, b.At, b.CallID = ws, time.UnixMilli(2000), "C2"
	s.Enqueue(a)
	s.Enqueue(b)
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	return dbPath, ws
}

func TestListNewestFirstWithGap(t *testing.T) {
	dbPath, ws := seedStore(t)
	r, err := OpenReadOnlyAt(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	es, err := r.List(Filter{Workspace: ws, Limit: 10})
	if err != nil || len(es) != 2 {
		t.Fatalf("List = %v, %v", es, err)
	}
	if es[0].At.UnixMilli() != 2000 || !es[0].GapBefore || es[1].GapBefore {
		t.Fatalf("order/gap wrong: %+v", es)
	}
	_, diff, err := r.Get(es[1].Seq)
	if err != nil || diff == "" {
		t.Fatalf("Get diff = %q, %v", diff, err)
	}
}

func TestOverflowMarkerDoesNotFabricateAGap(t *testing.T) {
	ws := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "h.db")
	s, err := Open(dbPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(ws, "f")
	first := item(OpUpdate, f, []byte("1\n"), []byte("2\n"))
	first.Workspace = ws
	second := marker(item(OpUpdate, f, []byte("2\n"), []byte("3\n"))) // overflowed row
	second.Workspace, second.At = ws, first.At.Add(time.Millisecond)
	s.Enqueue(first)
	s.Enqueue(second)
	_ = s.Close(context.Background())
	r, _ := OpenReadOnlyAt(dbPath)
	defer r.Close()
	es, _ := r.List(Filter{Workspace: ws, Limit: 10})
	for _, e := range es {
		if e.GapBefore {
			t.Fatalf("overflow produced a fabricated gap: %+v", es)
		}
	}
}

func TestOpenReadOnlyRefusesNewerSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.db")
	db, _ := sqlitex.Open(p, sqlitex.Options{MaxOpenConns: 1})
	_ = sqlitex.StampVersion(db, SchemaVersion+1)
	db.Close()
	if _, err := OpenReadOnlyAt(p); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("err = %v", err)
	}
}
