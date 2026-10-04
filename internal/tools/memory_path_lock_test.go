package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/memory"
)

// assertWaitsForPathLock runs fn while the test holds path's write lock and
// fails if fn finishes before the lock is released: a writer that does not take
// the lock can interleave with edit_file on the same file, and its history row
// then pairs a Before and an After that belong to different writes.
func assertWaitsForPathLock(t *testing.T, path string, fn func() error) {
	t.Helper()
	unlock := lockPath(path)
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("finished while another writer held the lock on %s (err: %v)", path, err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWriteMemoryTakesThePathLock(t *testing.T) {
	ws := t.TempDir()
	var f fakeHistory
	wm := NewWriteMemory(func(context.Context) string { return ws }).WithHistory(f.record, nil)
	path, _ := memory.Path(ws, "locked")
	args, _ := json.Marshal(map[string]any{"name": "locked", "content": "body\n"})
	assertWaitsForPathLock(t, path, func() error {
		_, err := wm.Execute(context.Background(), args)
		return err
	})
	if len(f.all()) != 1 {
		t.Fatalf("want one history row, got %d", len(f.all()))
	}
}

func TestDeleteMemoryTakesThePathLock(t *testing.T) {
	ws := t.TempDir()
	ctx := context.Background()
	wm := NewWriteMemory(func(context.Context) string { return ws })
	args, _ := json.Marshal(map[string]any{"name": "gone", "content": "body\n"})
	if _, err := wm.Execute(ctx, args); err != nil {
		t.Fatal(err)
	}
	var f fakeHistory
	dm := NewDeleteMemory(func(context.Context) string { return ws }).WithHistory(f.record, nil)
	path, _ := memory.Path(ws, "gone")
	del, _ := json.Marshal(map[string]any{"name": "gone"})
	assertWaitsForPathLock(t, path, func() error {
		_, err := dm.Execute(ctx, del)
		return err
	})
	if len(f.all()) != 1 {
		t.Fatalf("want one history row, got %d", len(f.all()))
	}
}
