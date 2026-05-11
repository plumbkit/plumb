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
      "description": "Symbol name or substring to search for (case-insensitive)"
    },
    "uri": {
      "type": "string",
      "description": "Document to search within (file:// URI). Required."
    }
  },
  "required": ["query", "uri"]
}`)

// FindSymbol searches for symbols by name within a single document. For
// workspace-wide search, use workspace_symbols.
type FindSymbol struct {
	client lsp.LSPClient
	cache  *cache.Cache
	ttl    time.Duration
}

// NewFindSymbol creates a FindSymbol tool. Pass a nil cache to disable caching.
func NewFindSymbol(client lsp.LSPClient, c *cache.Cache, ttl time.Duration) *FindSymbol {
	return &FindSymbol{client: client, cache: c, ttl: ttl}
}

func (t *FindSymbol) Name() string             { return "find_symbol" }
func (t *FindSymbol) InputSchema() json.RawMessage { return findSymbolSchema }
func (t *FindSymbol) Description() string {
	return "Search for symbols (functions, types, variables, classes) by name within a single document. Returns names, kinds, and line numbers. Matching is case-insensitive substring against the symbol name. For workspace-wide search, use workspace_symbols instead."
}

type findSymbolArgs struct {
	Query string `json:"query"`
	URI   string `json:"uri"`
}

func (t *FindSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a findSymbolArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("find_symbol: invalid arguments: %w", err)
	}
	if a.Query == "" {
		return "", fmt.Errorf("find_symbol: query must not be empty")
	}
	if a.URI == "" {
		return "", fmt.Errorf("find_symbol: uri is required (use workspace_symbols for workspace-wide search)")
	}
	return t.inDocument(ctx, a.URI, a.Query)
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
