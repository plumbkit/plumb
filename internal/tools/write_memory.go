package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/plumbkit/plumb/internal/memory"
)

type writeMemoryTool struct {
	ws      WorkspaceFn
	guard   BoundaryGuard
	indexFn func() *memory.Index
}

func NewWriteMemory(ws WorkspaceFn) *writeMemoryTool { return &writeMemoryTool{ws: ws} }

func (t *writeMemoryTool) WithBoundary(guard BoundaryGuard) *writeMemoryTool {
	t.guard = guard
	return t
}

// WithIndex wires the per-connection memory FTS index so writes keep it current.
func (t *writeMemoryTool) WithIndex(fn func() *memory.Index) *writeMemoryTool {
	t.indexFn = fn
	return t
}

func (*writeMemoryTool) Name() string { return "write_memory" }

func (*writeMemoryTool) Description() string {
	return `Write or overwrite a memory in a workspace's .plumb/memories/ directory.

The memory is a markdown file at <workspace>/.plumb/memories/<name>.md. If 'description' or 'paths' is provided, frontmatter is prepended automatically — list_memories will surface the description, and relevant_memories / hint injection use paths globs to attach the memory to files.

Memory names must match [A-Za-z0-9_-]+. Choose specific names that describe the memory's topic (e.g. 'auth-architecture', 'test-conventions', 'gotchas-cache-invalidation').`
}

func (*writeMemoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"name":{"type":"string","description":"Memory name (alphanumeric, _, - only)."},
			"content":{"type":"string","description":"Markdown body to save."},
			"description":{"type":"string","description":"One-line summary (optional). Stored as frontmatter."},
			"paths":{"type":"array","items":{"type":"string"},"description":"Optional workspace-relative file globs this memory applies to, e.g. internal/auth/** or cmd/server/*.go. Stored as frontmatter and used by relevant_memories plus hint injection."},
			"workspace":{"type":"string","description":"Absolute workspace path. Defaults to the daemon's resolved workspace."}
		},
		"required":["name","content"],
  "additionalProperties": false
}`)
}

func (t *writeMemoryTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name        string   `json:"name"`
		Content     string   `json:"content"`
		Description string   `json:"description"`
		Paths       []string `json:"paths"`
		Workspace   string   `json:"workspace"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.Name == "" {
		return "", fmt.Errorf("`name` is required")
	}
	if a.Content == "" {
		return "", fmt.Errorf("`content` is required")
	}
	ws := resolveWorkspace(a.Workspace, t.ws)
	if ws == "" {
		return "", noWorkspaceError()
	}
	if err := t.guard.check(ws); err != nil {
		return "", fmt.Errorf("write_memory: %w", err)
	}
	if err := memory.WriteIndexedWithOptions(resolveMemoryIndex(t.indexFn, ws), ws, a.Name, a.Content, memory.WriteOptions{Description: a.Description, Paths: a.Paths}); err != nil {
		return "", err
	}
	path, _ := memory.Path(ws, a.Name)
	return fmt.Sprintf("Memory saved to %s", path), nil
}
