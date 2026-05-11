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

var findSymbolSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Symbol name or substring to search for"
    },
    "uri": {
      "type": "string",
      "description": "Limit search to this document (file:// URI). Omit for workspace-wide search."
    }
  },
  "required": ["query"]
}`)

// FindSymbol searches for symbols by name across the workspace or within a document.
type FindSymbol struct {
	client lsp.LSPClient
	cache  *cache.Cache
	ttl    time.Duration
	ws     WorkspaceFn // used to filter dependency-cache hits from workspace queries
}

// NewFindSymbol creates a FindSymbol tool. Pass a nil cache to disable caching.
// ws may be nil to skip workspace-scoping (useful in tests).
func NewFindSymbol(client lsp.LSPClient, c *cache.Cache, ttl time.Duration, ws WorkspaceFn) *FindSymbol {
	return &FindSymbol{client: client, cache: c, ttl: ttl, ws: ws}
}

func (t *FindSymbol) Name() string             { return "find_symbol" }
func (t *FindSymbol) InputSchema() json.RawMessage { return findSymbolSchema }
func (t *FindSymbol) Description() string {
	return "Search for symbols (functions, types, variables, classes) by name across the workspace or within a single document. Returns names, kinds, and source locations."
}

type findSymbolArgs struct {
	Query string `json:"query"`
	URI   string `json:"uri,omitempty"`
}

func (t *FindSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a findSymbolArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("find_symbol: invalid arguments: %w", err)
	}
	if a.Query == "" {
		return "", fmt.Errorf("find_symbol: query must not be empty")
	}
	if a.URI != "" {
		return t.inDocument(ctx, a.URI, a.Query)
	}
	return t.inWorkspace(ctx, a.Query)
}

func (t *FindSymbol) inWorkspace(ctx context.Context, query string) (string, error) {
	key := "wsSymbols:" + query
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			return v.(string), nil
		}
	}

	syms, err := t.client.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolParams{Query: query})
	if err != nil {
		return "", fmt.Errorf("find_symbol: %w", err)
	}
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
		result = fmt.Sprintf("No symbols found matching %q.", query)
	} else {
		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d symbol(s) matching %q:\n\n", len(syms), query)
		for _, s := range syms {
			fmt.Fprintf(&sb, "- %s (%s) at %s:%d\n",
				s.Name, symbolKindName(s.Kind),
				s.Location.URI, s.Location.Range.Start.Line+1)
		}
		result = sb.String()
	}
	t.cacheSet(key, result)
	return result, nil
}

func (t *FindSymbol) inDocument(ctx context.Context, uri, query string) (string, error) {
	// Cache the full symbol list per document; filtering is client-side.
	key := uri + ":docSymbols"
	var syms []protocol.DocumentSymbol

	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			syms = v.([]protocol.DocumentSymbol)
		}
	}
	if syms == nil {
		var err error
		syms, err = t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		})
		if err != nil {
			return "", fmt.Errorf("find_symbol: %w", err)
		}
		if t.cache != nil {
			t.cache.Set(key, syms, t.ttl)
		}
	}

	matches := flatFilterSymbols(syms, query)
	if len(matches) == 0 {
		return fmt.Sprintf("No symbols matching %q in %s.", query, uri), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Symbols matching %q in %s:\n\n", query, uri)
	for _, s := range matches {
		fmt.Fprintf(&sb, "- %s (%s) at line %d\n",
			s.Name, symbolKindName(s.Kind), s.Range.Start.Line+1)
	}
	return sb.String(), nil
}

func (t *FindSymbol) cacheSet(key, value string) {
	if t.cache != nil {
		t.cache.Set(key, value, t.ttl)
	}
}

// flatFilterSymbols walks the symbol tree and returns all nodes whose name
// contains query (case-insensitive).
func flatFilterSymbols(syms []protocol.DocumentSymbol, query string) []protocol.DocumentSymbol {
	q := strings.ToLower(query)
	var out []protocol.DocumentSymbol
	var walk func([]protocol.DocumentSymbol)
	walk = func(ss []protocol.DocumentSymbol) {
		for _, s := range ss {
			if strings.Contains(strings.ToLower(s.Name), q) {
				out = append(out, s)
			}
			walk(s.Children)
		}
	}
	walk(syms)
	return out
}

func symbolKindName(k protocol.SymbolKind) string {
	switch k {
	case protocol.SKFile:
		return "File"
	case protocol.SKModule:
		return "Module"
	case protocol.SKNamespace:
		return "Namespace"
	case protocol.SKPackage:
		return "Package"
	case protocol.SKClass:
		return "Class"
	case protocol.SKMethod:
		return "Method"
	case protocol.SKProperty:
		return "Property"
	case protocol.SKField:
		return "Field"
	case protocol.SKConstructor:
		return "Constructor"
	case protocol.SKEnum:
		return "Enum"
	case protocol.SKInterface:
		return "Interface"
	case protocol.SKFunction:
		return "Function"
	case protocol.SKVariable:
		return "Variable"
	case protocol.SKConstant:
		return "Constant"
	case protocol.SKStruct:
		return "Struct"
	case protocol.SKEnumMember:
		return "EnumMember"
	case protocol.SKTypeParameter:
		return "TypeParameter"
	default:
		return fmt.Sprintf("Kind(%d)", int(k))
	}
}
