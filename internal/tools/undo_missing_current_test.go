package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
)

// A force undo of a file deleted since plumb wrote it restores the file. Its
// before-side is "absent", not "an existing empty file": recording the latter
// fabricates content the file never had and breaks the next gap check.
func TestForceUndoOfADeletedFileRecordsAnAbsentBefore(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "u.txt")
	if err := os.WriteFile(p, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var f fakeHistory
	deps := undoDeps()
	deps.HistoryFn = f.record
	deps.ShowWriteDiff = true
	ctx := context.Background()
	if _, err := NewWriteFile(deps).Execute(ctx, mustJSON(map[string]any{"file_path": p, "content": "v2\n"})); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := NewUndoEdit(deps).Execute(ctx, mustJSON(map[string]any{"file_path": p, "force": true})); err != nil {
		t.Fatal(err)
	}
	cs := f.all()
	if len(cs) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(cs))
	}
	rev := cs[1]
	if rev.Op != history.OpRevert || rev.Before.Exists || string(rev.After.Content) != "v1\n" {
		t.Fatalf("want a revert from an absent file to v1, got Before.Exists=%v %+v", rev.Before.Exists, rev)
	}
	if got, _ := os.ReadFile(p); string(got) != "v1\n" {
		t.Fatalf("file not restored: %q", got)
	}
}
