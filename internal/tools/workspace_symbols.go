package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

var workspaceSymbolsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Symbol name or substring to search for across the entire workspace"
    }
  },
  "required": ["query"]
}`)

// WorkspaceSymbols searches for symbols by name across the entire workspace.
type WorkspaceSymbols struct {
	client lsp.Client
	cache  *cache.Cache
	ttl    time.Duration
	ws     WorkspaceFn // used to filter out dependency-cache hits
}

// NewWorkspaceSymbols creates a WorkspaceSymbols tool. ws may be nil, in
// which case no workspace-scoping filter is applied.
func NewWorkspaceSymbols(client lsp.Client, c *cache.Cache, ttl time.Duration, ws WorkspaceFn) *WorkspaceSymbols {
	return &WorkspaceSymbols{client: client, cache: c, ttl: ttl, ws: ws}
}

func (t *WorkspaceSymbols) Name() string                 { return "workspace_symbols" }
func (t *WorkspaceSymbols) InputSchema() json.RawMessage { return workspaceSymbolsSchema }
func (t *WorkspaceSymbols) Description() string {
	return "No native Claude Code equivalent. " +
		"Search for symbols (functions, types, variables, constants) by name or substring across the entire workspace — instant, uses the LSP index. " +
		"Prefer this over search_in_files or grep when looking up a symbol by name. " +
		"Returns names, kinds, and source locations."
}

type workspaceSymbolsArgs struct {
	Query string `json:"query"`
}

func (t *WorkspaceSymbols) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a workspaceSymbolsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("workspace_symbols: invalid arguments: %w", err)
	}
	if a.Query == "" {
		return "", fmt.Errorf("workspace_symbols: query must not be empty")
	}

	key := "wsSymbols:" + a.Query
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			return v.(string), nil
		}
	}

	syms, err := t.client.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolParams{Query: a.Query})
	if err != nil {
		return "", fmt.Errorf("workspace_symbols: %w", err)
	}

	// Drop dependency-cache and stdlib hits so results stay focused on the
	// user's own code.
	if t.ws != nil {
		ws := t.ws()
		filtered := syms[:0]
		for _, s := range syms {
			if isInWorkspace(s.Location.URI, ws) {
				filtered = append(filtered, s)
			}
		}
		syms = filtered
	}

	var result string
	if len(syms) == 0 {
		result = fmt.Sprintf("No symbols found matching %q.", a.Query)
	} else {
		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d symbol(s) matching %q:\n\n", len(syms), a.Query)
		for _, s := range syms {
			fmt.Fprintf(&sb, "- %s (%s) at %s:%d\n",
				s.Name, symbolKindName(s.Kind),
				s.Location.URI, s.Location.Range.Start.Line+1)
		}
		result = sb.String()
	}

	if t.cache != nil {
		t.cache.Set(key, result, t.ttl)
	}
	return result, nil
}
