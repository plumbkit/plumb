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
	// deps carries the response policy (the show-diff and relay knobs, and the
	// sensitive-path resolver) for the diff this tool now appends. A zero
	// WriteDeps renders nothing, which is what a tool constructed without it
	// should do.
	deps WriteDeps
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

// WithWriteDeps wires the response-diff policy, so a memory write reports what it
// changed the same way the file tools do.
func (t *writeMemoryTool) WithWriteDeps(deps WriteDeps) *writeMemoryTool {
	t.deps = deps
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
	return "Write or overwrite a memory: a markdown file at <workspace>/.plumb/memories/<name>.md. description and paths become frontmatter, so list_memories shows the summary and relevant_memories and hints attach it to matching files. Names match [A-Za-z0-9_-]+; choose a specific topic name such as 'auth-architecture' or 'gotchas-cache-invalidation'."
}

func (*writeMemoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"name":{"type":"string","description":"Memory name (alphanumeric, _, - only)."},
			"content":{"type":"string","description":"Markdown body to save."},
			"description":{"type":"string","description":"One-line summary, stored as frontmatter."},
			"paths":{"type":"array","items":{"type":"string"},"description":"Workspace-relative globs this memory applies to, e.g. internal/auth/**; drives relevant_memories and hints."},
			"workspace":{"type":"string","description":"Absolute workspace path (default: the resolved workspace)."}
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
	// The path lock every plumb writer takes: an edit_file on the memory file, or
	// a second write_memory, cannot land between the Before read, the write and
	// the After read, so the history row pairs sides of one write.
	unlock := lockPath(path)
	defer unlock()
	// The file's bytes feed the history row AND the response diff, so they are
	// read when EITHER wants them — the same union gate the file tools use. With
	// both off, nothing is read at all.
	want := t.historyOn() || t.deps.showWriteDiff()
	// A side that cannot be read is unknown, not absent: recording it as absent
	// would fabricate history and render content nobody saw, so an unreadable
	// side records and shows nothing (writeAgentConfig does the same).
	before := history.Side{}
	readable := true
	if want {
		s, err := history.SideFromFile(path)
		before, readable = s, err == nil
	}
	if err := memory.WriteIndexedWithOptions(resolveMemoryIndex(t.indexFn, ws), ws, a.Name, a.Content, memory.WriteOptions{Description: a.Description, Paths: a.Paths}); err != nil {
		return "", err
	}
	after := history.Side{}
	op := history.OpUpdate
	if want && readable {
		s, err := history.SideFromFile(path)
		after, readable = s, err == nil
		if !before.Exists {
			op = history.OpCreate
		}
	}
	if !readable {
		return "Memory saved to " + path, nil
	}
	t.recordHistory(ctx, history.Change{Op: op, Tool: "write_memory", Path: path, Before: before, After: after})
	return "Memory saved to " + path + ResponseDiffSuffix(ctx, t.deps, path, before, after), nil
}
