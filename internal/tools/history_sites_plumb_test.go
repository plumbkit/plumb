package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/memory"
)

func TestWriteMemoryRecordsCreateThenUpdateWithDiskBytes(t *testing.T) {
	ws := t.TempDir()
	var f fakeHistory
	wm := NewWriteMemory(func(context.Context) string { return ws }).
		WithHistory(f.record, nil)

	ctx := context.Background()
	// 1. Create memory
	argsCreate, _ := json.Marshal(map[string]any{
		"name":        "arch",
		"content":     "initial content\n",
		"description": "Architecture doc",
	})
	if _, err := wm.Execute(ctx, argsCreate); err != nil {
		t.Fatalf("write_memory create: %v", err)
	}

	memPath, _ := memory.Path(ws, "arch")
	diskBytes, err := os.ReadFile(memPath)
	if err != nil {
		t.Fatalf("read memory file: %v", err)
	}

	cs := f.all()
	if len(cs) != 1 {
		t.Fatalf("expected 1 change, got %d", len(cs))
	}
	c1 := cs[0]
	if c1.Op != history.OpCreate {
		t.Errorf("Op = %v, want OpCreate", c1.Op)
	}
	if c1.Tool != "write_memory" {
		t.Errorf("Tool = %q, want write_memory", c1.Tool)
	}
	if c1.Before.Exists {
		t.Errorf("Before.Exists should be false")
	}
	if !c1.After.Exists || string(c1.After.Content) != string(diskBytes) {
		t.Errorf("After content mismatch: got %q, want disk %q", string(c1.After.Content), string(diskBytes))
	}

	// 2. Overwrite memory
	argsUpdate, _ := json.Marshal(map[string]any{
		"name":        "arch",
		"content":     "updated content\n",
		"description": "Updated doc",
	})
	if _, err := wm.Execute(ctx, argsUpdate); err != nil {
		t.Fatalf("write_memory update: %v", err)
	}

	diskBytes2, err := os.ReadFile(memPath)
	if err != nil {
		t.Fatalf("read memory file after update: %v", err)
	}

	cs = f.all()
	if len(cs) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(cs))
	}
	c2 := cs[1]
	if c2.Op != history.OpUpdate {
		t.Errorf("Op = %v, want OpUpdate", c2.Op)
	}
	if c2.Tool != "write_memory" {
		t.Errorf("Tool = %q, want write_memory", c2.Tool)
	}
	if !c2.Before.Exists || string(c2.Before.Content) != string(diskBytes) {
		t.Errorf("Before content mismatch: got %q, want %q", string(c2.Before.Content), string(diskBytes))
	}
	if !c2.After.Exists || string(c2.After.Content) != string(diskBytes2) {
		t.Errorf("After content mismatch: got %q, want disk %q", string(c2.After.Content), string(diskBytes2))
	}
}

func TestDeleteMemoryRecordsDelete(t *testing.T) {
	ws := t.TempDir()
	// Pre-create memory
	if err := memory.WriteIndexedWithOptions(nil, ws, "arch", "body", memory.WriteOptions{}); err != nil {
		t.Fatalf("pre-create memory: %v", err)
	}
	memPath, _ := memory.Path(ws, "arch")
	diskBytes, err := os.ReadFile(memPath)
	if err != nil {
		t.Fatalf("read memory: %v", err)
	}

	var f fakeHistory
	dm := NewDeleteMemory(func(context.Context) string { return ws }).
		WithHistory(f.record, nil)

	ctx := context.Background()
	args, _ := json.Marshal(map[string]any{"name": "arch"})
	if _, err := dm.Execute(ctx, args); err != nil {
		t.Fatalf("delete_memory: %v", err)
	}

	cs := f.all()
	if len(cs) != 1 {
		t.Fatalf("expected 1 change, got %d", len(cs))
	}
	c := cs[0]
	if c.Op != history.OpDelete {
		t.Errorf("Op = %v, want OpDelete", c.Op)
	}
	if c.Tool != "delete_memory" {
		t.Errorf("Tool = %q, want delete_memory", c.Tool)
	}
	if !c.Before.Exists || string(c.Before.Content) != string(diskBytes) {
		t.Errorf("Before content mismatch: got %q, want %q", string(c.Before.Content), string(diskBytes))
	}
	if c.After.Exists {
		t.Errorf("After.Exists should be false")
	}
}

func TestGitInitRecordsContextMdCreateOnlyWhenCreated(t *testing.T) {
	dir := t.TempDir()
	var f fakeHistory
	deps := WriteDeps{
		HistoryFn:   f.record,
		WorkspaceFn: func(context.Context) string { return dir },
	}
	gi := NewGitInit(deps)

	ctx := context.Background()
	args, _ := json.Marshal(map[string]any{
		"path":       dir,
		"init_plumb": true,
	})
	if _, err := gi.Execute(ctx, args); err != nil {
		t.Fatalf("git_init 1: %v", err)
	}

	cs := f.all()
	if len(cs) != 1 {
		t.Fatalf("expected 1 change, got %d", len(cs))
	}
	c := cs[0]
	if c.Op != history.OpCreate {
		t.Errorf("Op = %v, want OpCreate", c.Op)
	}
	if c.Tool != "git_init" {
		t.Errorf("Tool = %q, want git_init", c.Tool)
	}
	wantPath := filepath.Join(dir, ".plumb", "context.md")
	if c.Path != wantPath {
		t.Errorf("Path = %q, want %q", c.Path, wantPath)
	}
	if !c.After.Exists || string(c.After.Content) != plumbContextTemplate {
		t.Errorf("After.Content mismatch")
	}

	// Second run: context.md already exists, should not record anything
	if _, err := gi.Execute(ctx, args); err != nil {
		t.Fatalf("git_init 2: %v", err)
	}
	if len(f.all()) != 1 {
		t.Fatalf("expected still 1 change after second run, got %d", len(f.all()))
	}
}

func TestEnsureGitignoreEntriesIsNotRecorded(t *testing.T) {
	ws := t.TempDir()
	var f fakeHistory
	wm := NewWriteMemory(func(context.Context) string { return ws }).
		WithHistory(f.record, nil)

	ctx := context.Background()
	args, _ := json.Marshal(map[string]any{
		"name":    "arch",
		"content": "some content\n",
	})
	if _, err := wm.Execute(ctx, args); err != nil {
		t.Fatalf("write_memory: %v", err)
	}

	cs := f.all()
	for _, c := range cs {
		if strings.HasSuffix(c.Path, ".gitignore") {
			t.Errorf("found change on gitignore: %+v", c)
		}
	}
}
