package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/plumbkit/plumb/internal/memory"
)

type deleteMemoryTool struct {
	ws      WorkspaceFn
	guard   BoundaryGuard
	indexFn func() *memory.Index
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

func (t *deleteMemoryTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name      string `json:"name"`
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.Name == "" {
		return "", fmt.Errorf("`name` is required")
	}
	ws := resolveWorkspace(a.Workspace, t.ws)
	if ws == "" {
		return "", noWorkspaceError()
	}
	if err := t.guard.check(ws); err != nil {
		return "", fmt.Errorf("delete_memory: %w", err)
	}
	if err := memory.DeleteIndexed(resolveMemoryIndex(t.indexFn, ws), ws, a.Name); err != nil {
		return "", err
	}
	return fmt.Sprintf("Memory %q deleted from %s/.plumb/memories/", a.Name, ws), nil
}
