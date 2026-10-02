package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/memory"
)

type deleteMemoryTool struct {
	ws      WorkspaceFn
	guard   BoundaryGuard
	indexFn func() *memory.Index
	histFn  func(context.Context, history.Change)
	histOn  func() bool
}

func NewDeleteMemory(ws WorkspaceFn) *deleteMemoryTool { return &deleteMemoryTool{ws: ws} }

func (t *deleteMemoryTool) WithBoundary(guard BoundaryGuard) *deleteMemoryTool {
	t.guard = guard
	return t
}

// WithIndex wires the per-connection memory FTS index so deletes drop it too.
func (t *deleteMemoryTool) WithIndex(fn func() *memory.Index) *deleteMemoryTool {
	t.indexFn = fn
	return t
}

func (t *deleteMemoryTool) WithHistory(fn func(context.Context, history.Change), on func() bool) *deleteMemoryTool {
	t.histFn = fn
	t.histOn = on
	return t
}

func (t *deleteMemoryTool) historyOn() bool {
	return t.histFn != nil && (t.histOn == nil || t.histOn())
}

func (t *deleteMemoryTool) recordHistory(ctx context.Context, c history.Change) {
	if t.historyOn() {
		c.At, c.Kind = time.Now(), history.KindFile
		t.histFn(ctx, c)
	}
}

func (*deleteMemoryTool) Name() string { return "delete_memory" }

func (*deleteMemoryTool) Description() string {
	return `Delete a memory by name from a workspace's .plumb/memories/ directory.

Use only when explicitly asked, or when the memory has clearly become obsolete (e.g. it describes code that no longer exists).`
}

func (*deleteMemoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"name":{"type":"string","description":"Memory name to delete."},
			"workspace":{"type":"string","description":"Absolute workspace path. Defaults to the daemon's resolved workspace."}
		},
		"required":["name"],
  "additionalProperties": false
}`)
}

func (t *deleteMemoryTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name      string `json:"name"`
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.Name == "" {
		return "", errors.New("`name` is required")
	}
	ws := resolveWorkspace(ctx, a.Workspace, t.ws)
	if ws == "" {
		return "", noWorkspaceError()
	}
	if err := t.guard.check(ctx, ws); err != nil {
		return "", fmt.Errorf("delete_memory: %w", err)
	}
	path, _ := memory.Path(ws, a.Name)
	// Under the path lock, so no other plumb writer lands between the Before
	// read and the delete.
	unlock := lockPath(path)
	defer unlock()
	before := history.Side{}
	if t.historyOn() {
		if s, err := history.SideFromFile(path); err == nil {
			before = s
		}
	}
	if err := memory.DeleteIndexed(resolveMemoryIndex(t.indexFn, ws), ws, a.Name); err != nil {
		return "", err
	}
	if t.historyOn() {
		t.recordHistory(ctx, history.Change{Op: history.OpDelete, Tool: "delete_memory", Path: path, Before: before})
	}
	return fmt.Sprintf("Memory %q deleted from %s/.plumb/memories/", a.Name, ws), nil
}
