package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/topology"
)

type readMemoryTool struct {
	ws      WorkspaceFn
	guard   BoundaryGuard
	indexFn func() *memory.Index
	topoFn  func() *topology.Store
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

// WithTopology wires the topology store so a generated memory's referenced
// symbols can be stale-checked against the live code map.
func (t *readMemoryTool) WithTopology(fn func() *topology.Store) *readMemoryTool {
	t.topoFn = fn
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

func (t *readMemoryTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
		if t.topoFn != nil {
			content += staleSymbolsNote(ctx, t.topoFn(), rec)
		}
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

// staleSymbolsMax caps the per-read topology lookups so a memory with a huge
// provenance trail cannot turn one read into dozens of index queries.
const staleSymbolsMax = 8

// staleSymbolsNote checks a memory's referenced source_symbols against the
// live topology index and reports the ones that no longer exist — a renamed
// or deleted symbol means the memory may describe code that is gone. Silent
// (returns "") when there is no store, no referenced symbols, or every symbol
// still resolves; the check never fails the read.
func staleSymbolsNote(ctx context.Context, store *topology.Store, rec memory.Record) string {
	if store == nil || len(rec.SourceSymbols) == 0 {
		return ""
	}
	syms := rec.SourceSymbols
	if len(syms) > staleSymbolsMax {
		syms = syms[:staleSymbolsMax]
	}
	var missing []string
	for _, sym := range syms {
		nodes, err := store.ResolveNodes(ctx, sym, topology.NodeHint{})
		if err == nil && len(nodes) == 0 {
			missing = append(missing, sym)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("\n[stale-check] referenced symbols no longer in the code map: %s — this memory may describe code that has moved or been removed.",
		strings.Join(missing, ", "))
}
