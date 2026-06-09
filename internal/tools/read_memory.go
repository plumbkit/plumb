package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/memory"
)

type readMemoryTool struct {
	ws      WorkspaceFn
	guard   BoundaryGuard
	indexFn func() *memory.Index
}

func NewReadMemory(ws WorkspaceFn) *readMemoryTool { return &readMemoryTool{ws: ws} }

func (t *readMemoryTool) WithBoundary(guard BoundaryGuard) *readMemoryTool {
	t.guard = guard
	return t
}

// WithIndex wires the memory index so a read bumps the memory's last-used time
// (recency nudges ranking).
func (t *readMemoryTool) WithIndex(fn func() *memory.Index) *readMemoryTool {
	t.indexFn = fn
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
	content, err := memory.Read(ws, a.Name)
	if err != nil {
		return "", err
	}
	if rec, merr := memory.ReadMeta(ws, a.Name); merr == nil {
		content += memoryProvenanceFooter(rec)
	}
	if ix := resolveMemoryIndex(t.indexFn, ws); ix != nil {
		_ = ix.TouchUsed(a.Name)
	}
	return content, nil
}

// memoryProvenanceFooter returns a compact footer describing how a generated
// memory came to exist. A user-authored memory (or one with no provenance)
// returns "".
func memoryProvenanceFooter(rec memory.Record) string {
	if rec.Confidence == "" || rec.Confidence == memory.ConfidenceUser {
		return ""
	}
	parts := []string{}
	if rec.SourceSession != "" {
		parts = append(parts, "session "+rec.SourceSession)
	}
	if len(rec.SourcePaths) > 0 {
		parts = append(parts, "touched "+strings.Join(rec.SourcePaths, ", "))
	}
	if !rec.CreatedAt.IsZero() {
		parts = append(parts, rec.CreatedAt.Format("2006-01-02"))
	}
	detail := ""
	if len(parts) > 0 {
		detail = " — " + strings.Join(parts, " · ")
	}
	return fmt.Sprintf("\n\n---\n[provenance] %s%s", rec.Confidence, detail)
}
