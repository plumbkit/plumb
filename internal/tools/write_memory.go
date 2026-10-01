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

type writeMemoryTool struct {
	ws      WorkspaceFn
	guard   BoundaryGuard
	indexFn func() *memory.Index
	histFn  func(context.Context, history.Change)
	histOn  func() bool
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

func (t *writeMemoryTool) WithHistory(fn func(context.Context, history.Change), on func() bool) *writeMemoryTool {
	t.histFn = fn
	t.histOn = on
	return t
}

func (t *writeMemoryTool) historyOn() bool {
	return t.histFn != nil && (t.histOn == nil || t.histOn())
}

func (t *writeMemoryTool) recordHistory(ctx context.Context, c history.Change) {
	if t.historyOn() {
		c.At, c.Kind = time.Now(), history.KindFile
		t.histFn(ctx, c)
	}
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

func (t *writeMemoryTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
		return "", errors.New("`name` is required")
	}
	if a.Content == "" {
		return "", errors.New("`content` is required")
	}
	ws := resolveWorkspace(ctx, a.Workspace, t.ws)
	if ws == "" {
		return "", noWorkspaceError()
	}
	if err := t.guard.check(ctx, ws); err != nil {
		return "", fmt.Errorf("write_memory: %w", err)
	}
	path, _ := memory.Path(ws, a.Name)
	before := history.Side{}
	if t.historyOn() {
		if s, err := history.SideFromFile(path); err == nil {
			before = s
		}
	}
	if err := memory.WriteIndexedWithOptions(resolveMemoryIndex(t.indexFn, ws), ws, a.Name, a.Content, memory.WriteOptions{Description: a.Description, Paths: a.Paths}); err != nil {
		return "", err
	}
	// Memory writes have no per-path lock. Two concurrent write_memory calls
	// to one name are ordered by their ts_ms only.
	if t.historyOn() {
		if after, err := history.SideFromFile(path); err == nil {
			op := history.OpUpdate
			if !before.Exists {
				op = history.OpCreate
			}
			t.recordHistory(ctx, history.Change{Op: op, Tool: "write_memory", Path: path, Before: before, After: after})
		}
	}
	return "Memory saved to " + path, nil
}
