package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/memory"
)

// WorkspaceFn returns the daemon's currently-resolved workspace, or "" if
// none. The memory tools use it as a fallback when the caller doesn't pass
// an explicit `workspace` argument.
type WorkspaceFn func() string

type listMemoriesTool struct {
	ws    WorkspaceFn
	guard BoundaryGuard
}

func NewListMemories(ws WorkspaceFn) *listMemoriesTool { return &listMemoriesTool{ws: ws} }

func (t *listMemoriesTool) WithBoundary(guard BoundaryGuard) *listMemoriesTool {
	t.guard = guard
	return t
}

func (*listMemoriesTool) Name() string { return "list_memories" }

func (*listMemoriesTool) Description() string {
	return `List memories saved for a workspace.

Memories are markdown notes stored in <workspace>/.plumb/memories/<name>.md. They persist project-specific context — conventions, architectural decisions, gotchas — across MCP conversations. Each memory may have YAML frontmatter (name, description) used as a one-line summary in the listing.

If 'workspace' is omitted, the daemon's currently-resolved workspace is used.`
}

func (*listMemoriesTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"workspace":{"type":"string","description":"Absolute workspace path. Defaults to the daemon's resolved workspace."}
		},
  "additionalProperties": false
}`)
}

func (t *listMemoriesTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Workspace string `json:"workspace"`
	}
	_ = json.Unmarshal(args, &a)
	ws := resolveWorkspace(a.Workspace, t.ws)
	if ws == "" {
		return "", noWorkspaceError()
	}
	if err := t.guard.check(ws); err != nil {
		return "", fmt.Errorf("list_memories: %w", err)
	}
	mems, err := memory.List(ws)
	if err != nil {
		return "", err
	}
	if len(mems) == 0 {
		return fmt.Sprintf("No memories yet in %s/.plumb/memories/.\nUse write_memory to save project notes.", ws), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d memory(ies) in %s/.plumb/memories/:\n\n", len(mems), ws)
	for _, m := range mems {
		fmt.Fprintf(&sb, "- %s", m.Name)
		if m.Description != "" {
			fmt.Fprintf(&sb, " — %s", m.Description)
		}
		fmt.Fprintf(&sb, "  (%d bytes)\n", m.SizeBytes)
	}
	return sb.String(), nil
}

func resolveWorkspace(explicit string, fallback WorkspaceFn) string {
	if explicit != "" {
		return explicit
	}
	if fallback != nil {
		return fallback()
	}
	return ""
}

func noWorkspaceError() error {
	return fmt.Errorf("no workspace resolved for this connection; call session_start (optionally with an absolute `workspace`) to attach. " +
		"If this session was working a moment ago, the daemon may have restarted (e.g. after a rebuild or upgrade), which clears the per-connection workspace pin — re-run session_start to re-attach. " +
		"You can also pass `workspace` explicitly, or call any path-bearing tool first to resolve it from a file URI")
}
