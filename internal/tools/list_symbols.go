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

var listSymbolsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "file:// URI of the document to outline"
    },
    "include_signatures": {
      "type": "boolean",
      "description": "When true, append the first non-blank, non-comment source line of each function, method, or constructor symbol below its entry. Useful for seeing parameter types and receiver types without reading the full file."
    }
  },
  "required": ["uri"],
  "additionalProperties": false
}`)

// ListSymbols returns the full symbol outline of a document in one call —
// names, kinds, line ranges, and children — so callers can target further
// queries without needing to know symbol names in advance.
//
// Concurrency: Execute is safe for concurrent use.
type ListSymbols struct {
	client  lsp.Client
	cache   *cache.Cache
	ttl     time.Duration
	timeout time.Duration
	topo    topologyStoreFn
}

// WithTopologyFallback wires the topology index as a fallback for when the
// language server errors or times out. Returns the tool for chaining.
func (t *ListSymbols) WithTopologyFallback(fn topologyStoreFn) *ListSymbols {
	t.topo = fn
	return t
}

func NewListSymbols(client lsp.Client, c *cache.Cache, ttl, timeout time.Duration) *ListSymbols {
	return &ListSymbols{client: client, cache: c, ttl: ttl, timeout: timeout}
}

func (t *ListSymbols) Name() string                 { return "list_symbols" }
func (t *ListSymbols) InputSchema() json.RawMessage { return listSymbolsSchema }
func (t *ListSymbols) Description() string {
	return "Return the complete symbol outline of a file: every function, type, method, field, " +
		"and constant with its kind and line range. Use this before explain_symbol or get_definition " +
		"to discover what a file contains without reading it. " +
		"Set include_signatures=true to append the declaration line of each function, method, or constructor " +
		"(shows parameter types and receiver types)."
}

// topologyFallback answers a file outline from the topology index. ok is false
// when topology is unavailable or has not indexed the file, so the caller
// surfaces the original LSP error instead.
func (t *ListSymbols) topologyFallback(ctx context.Context, uri string) (string, bool) {
	store := activeTopology(t.topo)
	if store == nil {
		return "", false
	}
	nodes, err := store.SymbolsInFile(ctx, uri)
	if err != nil || len(nodes) == 0 {
		return "", false
	}
	return formatTopologyOutline(uri, nodes), true
}

func (t *ListSymbols) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		URI               string `json:"uri"`
		IncludeSignatures bool   `json:"include_signatures"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("list_symbols: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return "", fmt.Errorf("list_symbols: uri is required")
	}

	key := a.URI + ":docSymbols"
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			return formatSymbolTree(a.URI, v.([]protocol.DocumentSymbol), a.IncludeSignatures), nil
		}
	}

	lspCtx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()
	syms, err := t.client.DocumentSymbols(lspCtx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
	})
	if err != nil {
		if out, ok := t.topologyFallback(ctx, a.URI); ok {
			return out, nil
		}
		return "", lspTimeoutErr("list_symbols", t.timeout, err)
	}
	if t.cache != nil {
		t.cache.Set(key, syms, t.ttl)
	}
	return formatSymbolTree(a.URI, syms, a.IncludeSignatures), nil
}

func formatSymbolTree(uri string, syms []protocol.DocumentSymbol, includeSignatures bool) string {
	if len(syms) == 0 {
		return fmt.Sprintf("No symbols found in %s.", uri)
	}
	var sb strings.Builder
	total := countSymbols(syms)
	fmt.Fprintf(&sb, "Symbols in %s (%d total)\n\n", uri, total)
	var lineTexts map[uint32]string
	if includeSignatures {
		fpath := strings.TrimPrefix(uri, "file://")
		lineTexts = readFileLines(fpath, collectStartLines(syms))
	}
	writeSymbols(&sb, syms, 0, lineTexts)
	return sb.String()
}

// isCallableKind reports whether a symbol kind warrants a signature annotation
// (function, method, or constructor). Non-callable kinds such as fields,
// constants, and type declarations do not get the → prefix.
func isCallableKind(kind protocol.SymbolKind) bool {
	return kind == protocol.SKFunction || kind == protocol.SKMethod || kind == protocol.SKConstructor
}

func collectStartLines(syms []protocol.DocumentSymbol) map[uint32]bool {
	lines := make(map[uint32]bool)
	var walk func([]protocol.DocumentSymbol)
	walk = func(ss []protocol.DocumentSymbol) {
		for _, s := range ss {
			if isCallableKind(s.Kind) {
				lines[s.Range.Start.Line] = true
			}
			walk(s.Children)
		}
	}
	walk(syms)
	return lines
}

func writeSymbols(sb *strings.Builder, syms []protocol.DocumentSymbol, depth int, lineTexts map[uint32]string) {
	indent := strings.Repeat("  ", depth)
	for _, s := range syms {
		start := s.Range.Start.Line + 1
		end := s.Range.End.Line + 1
		detail := ""
		if s.Detail != "" {
			detail = " " + s.Detail
		}
		if start == end {
			fmt.Fprintf(sb, "%s%s%s (%s) line %d\n", indent, s.Name, detail, symbolKindName(s.Kind), start)
		} else {
			fmt.Fprintf(sb, "%s%s%s (%s) lines %d–%d\n", indent, s.Name, detail, symbolKindName(s.Kind), start, end)
		}
		if lineTexts != nil && isCallableKind(s.Kind) {
			if sig, ok := lineTexts[s.Range.Start.Line]; ok {
				sig = strings.TrimSpace(sig)
				if sig != "" && !strings.HasPrefix(sig, "//") && !strings.HasPrefix(sig, "#") && !strings.HasPrefix(sig, "/*") {
					fmt.Fprintf(sb, "%s  → %s\n", indent, sig)
				}
			}
		}
		if len(s.Children) > 0 {
			writeSymbols(sb, s.Children, depth+1, lineTexts)
		}
	}
}

func countSymbols(syms []protocol.DocumentSymbol) int {
	n := len(syms)
	for _, s := range syms {
		n += countSymbols(s.Children)
	}
	return n
}
