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
    }
  },
  "required": ["uri"]
}`)

// ListSymbols returns the full symbol outline of a document in one call —
// names, kinds, line ranges, and children — so callers can target further
// queries without needing to know symbol names in advance.
//
// Concurrency: Execute is safe for concurrent use.
type ListSymbols struct {
	client lsp.LSPClient
	cache  *cache.Cache
	ttl    time.Duration
}

func NewListSymbols(client lsp.LSPClient, c *cache.Cache, ttl time.Duration) *ListSymbols {
	return &ListSymbols{client: client, cache: c, ttl: ttl}
}

func (t *ListSymbols) Name() string             { return "list_symbols" }
func (t *ListSymbols) InputSchema() json.RawMessage { return listSymbolsSchema }
func (t *ListSymbols) Description() string {
	return "Return the complete symbol outline of a file: every function, type, method, field, " +
		"and constant with its kind and line range. Use this before explain_symbol or get_definition " +
		"to discover what a file contains without reading it."
}

func (t *ListSymbols) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		URI string `json:"uri"`
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
			return formatSymbolTree(a.URI, v.([]protocol.DocumentSymbol)), nil
		}
	}

	syms, err := t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
	})
	if err != nil {
		return "", fmt.Errorf("list_symbols: %w", err)
	}
	if t.cache != nil {
		t.cache.Set(key, syms, t.ttl)
	}
	return formatSymbolTree(a.URI, syms), nil
}

func formatSymbolTree(uri string, syms []protocol.DocumentSymbol) string {
	if len(syms) == 0 {
		return fmt.Sprintf("No symbols found in %s.", uri)
	}
	var sb strings.Builder
	total := countSymbols(syms)
	fmt.Fprintf(&sb, "Symbols in %s (%d total)\n\n", uri, total)
	writeSymbols(&sb, syms, 0)
	return sb.String()
}

func writeSymbols(sb *strings.Builder, syms []protocol.DocumentSymbol, depth int) {
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
		if len(s.Children) > 0 {
			writeSymbols(sb, s.Children, depth+1)
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
