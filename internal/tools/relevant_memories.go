package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/memory"
)

type relevantMemoriesTool struct {
	ws    WorkspaceFn
	guard BoundaryGuard
}

func NewRelevantMemories(ws WorkspaceFn) *relevantMemoriesTool {
	return &relevantMemoriesTool{ws: ws}
}

func (t *relevantMemoriesTool) WithBoundary(guard BoundaryGuard) *relevantMemoriesTool {
	t.guard = guard
	return t
}

func (*relevantMemoriesTool) Name() string { return "relevant_memories" }

func (*relevantMemoriesTool) Description() string {
	return `Return memories whose frontmatter 'paths:' globs match the given file.

Memories can be auto-attached to specific parts of a project by adding a 'paths:' field to their frontmatter (e.g. 'paths: internal/auth/**, cmd/server/*.go'). This tool surfaces only the memories relevant to a given file — much smaller than list_memories when many memories exist.

Call this when starting work on a file to discover context the LLM should load before editing.`
}

func (*relevantMemoriesTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"File path. Either absolute or relative to the workspace."},
			"workspace":{"type":"string","description":"Absolute workspace path. Defaults to the daemon's resolved workspace."}
		},
		"required":["path"],
  "additionalProperties": false
}`)
}

func (t *relevantMemoriesTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path      string `json:"path"`
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("`path` is required")
	}
	ws := resolveWorkspace(a.Workspace, t.ws)
	if ws == "" {
		return "", noWorkspaceError()
	}
	if err := t.guard.check(ws); err != nil {
		return "", fmt.Errorf("relevant_memories: %w", err)
	}

	// Normalise to a workspace-relative path.
	rel := a.Path
	if filepath.IsAbs(a.Path) {
		if err := t.guard.check(a.Path); err != nil {
			return "", fmt.Errorf("relevant_memories: %w", err)
		}
		r, err := filepath.Rel(ws, a.Path)
		if err != nil || strings.HasPrefix(r, "..") {
			return fmt.Sprintf("path %s is not inside workspace %s", a.Path, ws), nil
		}
		rel = r
	}

	mems, err := memory.Relevant(ws, rel)
	if err != nil {
		return "", err
	}
	if len(mems) == 0 {
		return fmt.Sprintf("No path-scoped memories match %s.", rel), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d memor(ies) auto-attached to %s:\n\n", len(mems), rel)
	for _, m := range mems {
		fmt.Fprintf(&sb, "- %s", m.Name)
		if m.Description != "" {
			fmt.Fprintf(&sb, " — %s", m.Description)
		}
		fmt.Fprintf(&sb, "\n  matches: %s\n", strings.Join(m.Paths, ", "))
	}
	sb.WriteString("\nCall read_memory to load each.")
	return sb.String(), nil
}
