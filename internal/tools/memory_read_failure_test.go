package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/memory"
)

// unreadableMemory creates memory name in ws as a write-only file, so a write
// or delete succeeds while reading the file back fails.
func unreadableMemory(t *testing.T, ws, name string) string {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("root reads a write-only file")
	}
	path, err := memory.Path(ws, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old body\n"), 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	return path
}

// A side that cannot be read is unknown, not absent. Recording it as absent
// fabricates history (a create of a file that existed, a delete of one that
// had content) and renders a diff of content nobody saw; the write itself still
// succeeds and is reported.
func TestWriteMemoryWithAnUnreadableFileRecordsNoFabricatedSide(t *testing.T) {
	ws := t.TempDir()
	unreadableMemory(t, ws, "m")
	var f fakeHistory
	wm := NewWriteMemory(func(context.Context) string { return ws }).
		WithHistory(f.record, nil).WithWriteDeps(WriteDeps{ShowWriteDiff: true})
	args, _ := json.Marshal(map[string]any{"name": "m", "content": "new body\n"})
	out, err := wm.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if cs := f.all(); len(cs) != 0 {
		t.Fatalf("recorded %d rows from sides it could not read: %+v", len(cs), cs)
	}
	if strings.Contains(out, "@@") {
		t.Fatalf("rendered a diff of content it could not read:\n%s", out)
	}
	if !strings.HasPrefix(out, "Memory saved to ") {
		t.Fatalf("the write's own report was lost: %q", out)
	}
}

func TestDeleteMemoryWithAnUnreadableFileRecordsNoFabricatedSide(t *testing.T) {
	ws := t.TempDir()
	unreadableMemory(t, ws, "m")
	var f fakeHistory
	dm := NewDeleteMemory(func(context.Context) string { return ws }).
		WithHistory(f.record, nil).WithWriteDeps(WriteDeps{ShowWriteDiff: true})
	args, _ := json.Marshal(map[string]any{"name": "m"})
	out, err := dm.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if cs := f.all(); len(cs) != 0 {
		t.Fatalf("recorded %d rows from a side it could not read: %+v", len(cs), cs)
	}
	if strings.Contains(out, "@@") {
		t.Fatalf("rendered a diff of content it could not read:\n%s", out)
	}
}
