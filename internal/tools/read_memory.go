package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/plumbkit/plumb/internal/memory"
)

type readMemoryTool struct {
	ws    WorkspaceFn
	guard BoundaryGuard
}

func NewReadMemory(ws WorkspaceFn) *readMemoryTool { return &readMemoryTool{ws: ws} }

func (t *readMemoryTool) WithBoundary(guard BoundaryGuard) *readMemoryTool {
	t.guard = guard
	return t
}

func (*readMemoryTool) Name() string { return "read_memory" }

func (*readMemoryTool) Description() string {
	return `Read a saved memory by name from a workspace's .plumb/memories/ directory.

Returns the full markdown content (including any frontmatter). Use list_memories first to discover what memories exist.`
}

func (*readMemoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"name":{"type":"string","description":"Memory name (alphanumeric, _, - only)."},
			"workspace":{"type":"string","description":"Absolute workspace path. Defaults to the daemon's resolved workspace."}
		},
		"required":["name"],
  "additionalProperties": false
}`)
}

func (t *readMemoryTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
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
		return "", fmt.Errorf("read_memory: %w", err)
	}
	return memory.Read(ws, a.Name)
}
