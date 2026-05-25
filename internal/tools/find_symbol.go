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
      "description": "Document to search within (absolute path or file:// URI). Required."
    }
  },
  "required": ["query", "uri"],
  "additionalProperties": false
}`)

// FindSymbol searches for symbols by name within a single document. For
// workspace-wide search, use workspace_symbols.
type FindSymbol struct {
	client  lsp.Client
	cache   *cache.Cache
	ttl     time.Duration
	timeout time.Duration
	topo    topologyStoreFn
}

// WithTopologyFallback wires the topology index as a fallback for when the
// language server errors or times out. Returns the tool for chaining.
func (t *FindSymbol) WithTopologyFallback(fn topologyStoreFn) *FindSymbol {
	t.topo = fn
	return t
}

// NewFindSymbol creates a FindSymbol tool. Pass a nil cache to disable caching.
func NewFindSymbol(client lsp.Client, c *cache.Cache, ttl, timeout time.Duration) *FindSymbol {
	return &FindSymbol{client: client, cache: c, ttl: ttl, timeout: timeout}
}

func (t *FindSymbol) Name() string                 { return "find_symbol" }
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
	a.URI = toFileURI(a.URI)
	lspCtx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()
	out, err := t.inDocument(lspCtx, a.URI, a.Query)
	if err != nil {
		if fb, ok := t.topologyFallback(ctx, a.URI, a.Query); ok {
			return fb, nil
		}
		return "", err
	}
	return out, nil
}

// topologyFallback answers an in-file symbol search from the topology index.
// ok is false when topology is unavailable or has not indexed the file, so the
// caller surfaces the original LSP error instead.
func (t *FindSymbol) topologyFallback(ctx context.Context, uri, query string) (string, bool) {
	store := activeTopology(t.topo)
	if store == nil {
		return "", false
	}
	nodes, err := store.SymbolsInFile(ctx, uri)
	if err != nil || len(nodes) == 0 {
		return "", false
	}
	matches := filterTopologyByName(nodes, query)
	return formatTopologyMatches(fmt.Sprintf("Symbols matching %q in %s", query, uri), matches), true
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
			return "", lspTimeoutErr("find_symbol", t.timeout, err)
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

// resolveSymbolsByName returns all symbols in the tree matching name.
// For dotted names like "ReceiverType.MethodName" it searches children of the
// named parent. For plain names it matches at any depth.
func resolveSymbolsByName(syms []protocol.DocumentSymbol, name string) []protocol.DocumentSymbol {
	if parent, child, ok := strings.Cut(name, "."); ok {
		var out []protocol.DocumentSymbol
		for _, s := range syms {
			if s.Name == parent {
				for _, c := range s.Children {
					if c.Name == child {
						out = append(out, c)
					}
				}
			}
		}
		return out
	}
	var out []protocol.DocumentSymbol
	var walk func([]protocol.DocumentSymbol)
	walk = func(ss []protocol.DocumentSymbol) {
		for _, s := range ss {
			if s.Name == name {
				out = append(out, s)
			}
			walk(s.Children)
		}
	}
	walk(syms)
	return out
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

var symbolKindNames = map[protocol.SymbolKind]string{
	protocol.SKFile:          "File",
	protocol.SKModule:        "Module",
	protocol.SKNamespace:     "Namespace",
	protocol.SKPackage:       "Package",
	protocol.SKClass:         "Class",
	protocol.SKMethod:        "Method",
	protocol.SKProperty:      "Property",
	protocol.SKField:         "Field",
	protocol.SKConstructor:   "Constructor",
	protocol.SKEnum:          "Enum",
	protocol.SKInterface:     "Interface",
	protocol.SKFunction:      "Function",
	protocol.SKVariable:      "Variable",
	protocol.SKConstant:      "Constant",
	protocol.SKStruct:        "Struct",
	protocol.SKEnumMember:    "EnumMember",
	protocol.SKTypeParameter: "TypeParameter",
}

func symbolKindName(k protocol.SymbolKind) string {
	if name, ok := symbolKindNames[k]; ok {
		return name
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}
