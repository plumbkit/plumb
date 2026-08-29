package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

var explainSymbolSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path of the document"
    },
    "line": {
      "type": "integer",
      "description": "Zero-based line number. Required when symbol_name is not provided.",
      "minimum": 0
    },
    "character": {
      "type": "integer",
      "description": "Zero-based character offset. Required when symbol_name is not provided.",
      "minimum": 0
    },
    "symbol_name": {
      "type": "string",
      "description": "Symbol name to look up instead of a position — PREFERRED over line/character. Accepts plain name or ReceiverType.MethodName form. plumb resolves it against the file's symbols, avoiding the off-by-one and 'no identifier found' errors of a hand-computed position. When provided, line and character are not needed."
    }
  },
  "required": ["uri"],
  "additionalProperties": false
}`)

// ExplainSymbol returns hover information (documentation, type signature) for
// the symbol at a given position, or by name.
type ExplainSymbol struct {
	client    lsp.Client
	cache     *cache.Cache
	ttl       time.Duration
	timeout   time.Duration
	warmup    LSPWarmupFn // optional; rewrites a cold-LSP failure into a still-warming advisory
	ws        WorkspaceFn // may be nil; anchors a workspace-relative uri to the pinned root
	contested ContestedFn // may be nil; refuses a relative uri once the pin is contested
}

// NewExplainSymbol creates an ExplainSymbol tool. Pass a nil cache to disable caching.
func NewExplainSymbol(client lsp.Client, c *cache.Cache, ttl, timeout time.Duration) *ExplainSymbol {
	return &ExplainSymbol{client: client, cache: c, ttl: ttl, timeout: timeout}
}

// WithLSPWarmup wires the warm-up probe so a failure against a still-warming
// server says so (and names what answers now) instead of showing the 0-based
// coordinate hint, which would mislead there. Nil-safe.
func (t *ExplainSymbol) WithLSPWarmup(fn LSPWarmupFn) *ExplainSymbol {
	t.warmup = fn
	return t
}

// WithWorkspace anchors a relative uri to the pinned workspace root. Nil-safe.
func (t *ExplainSymbol) WithWorkspace(ws WorkspaceFn) *ExplainSymbol {
	t.ws = ws
	return t
}

// WithContested wires the contested-pin reporter so a RELATIVE uri is refused
// once the pin is contested (issue #182). Nil-safe.
func (t *ExplainSymbol) WithContested(fn ContestedFn) *ExplainSymbol {
	t.contested = fn
	return t
}

func (t *ExplainSymbol) Name() string                 { return "explain_symbol" }
func (t *ExplainSymbol) InputSchema() json.RawMessage { return explainSymbolSchema }
func (t *ExplainSymbol) Description() string {
	return "Returns DOCUMENTATION and type information (LSP hover content: function signature, doc comment, often in Markdown) for the symbol at the given position or by name. " +
		"PREFER a name (uri + symbol_name) — plumb resolves the exact identifier position for you, " +
		"avoiding off-by-one errors; a raw file position (uri + line + character) is the fallback " +
		"and, when it lands off an identifier, is snapped to the enclosing symbol. " +
		"Use when you need to understand what a symbol is without navigating to its source. " +
		"For the file location of where the symbol is defined, use get_definition instead."
}

type explainSymbolArgs struct {
	URI        string  `json:"uri"`
	Line       *uint32 `json:"line"`
	Character  *uint32 `json:"character"`
	SymbolName string  `json:"symbol_name"`
}

// Execute's shape is intentionally near-identical to GetDefinition.Execute /
// CallHierarchy.Execute / TypeHierarchy.Execute — see the comment on
// GetDefinition.Execute for why.
//
//nolint:dupl // structurally identical by design across the four query tools, see get_definition.go
func (t *ExplainSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a explainSymbolArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("explain_symbol: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return "", errors.New("explain_symbol: uri must not be empty")
	}
	return executeLSPQuery(ctx, "explain_symbol", t.ws, t.contested, t.timeout, a.URI, a.SymbolName, a.Line, a.Character,
		func(ctx context.Context, uri string) (string, error) { return t.executeByName(ctx, uri, a.SymbolName) },
		func(ctx context.Context, uri string, line, character uint32) (string, error) {
			return t.executeByPosition(ctx, uri, line, character, true, "")
		},
	)
}

// executeByName resolves name against the file's document symbols and queries
// hover at the identifier position (SelectionRange.Start) — the same
// off-by-one-proof path find_references/get_definition/call_hierarchy use. A
// name matching several symbols renders every match in turn rather than
// erroring or guessing (never auto-pick among ambiguous candidates).
func (t *ExplainSymbol) executeByName(ctx context.Context, uri, name string) (string, error) {
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
			if werr := coldLSPWarmingErr("explain_symbol", t.warmup, uri); werr != nil {
				return "", werr
			}
			return "", lspTimeoutErr("explain_symbol", t.timeout, fmt.Errorf("resolving symbol %q: %w", name, err))
		}
		if t.cache != nil {
			t.cache.Set(key, syms, t.ttl)
		}
	}

	matches := resolveSymbolsByName(syms, name)
	if len(matches) == 0 {
		return fmt.Sprintf("No symbol named %q in %s.%s", name, uri, didYouMean(suggestSymbols(syms, name))), nil
	}
	if len(matches) == 1 {
		sym := matches[0]
		return t.executeByPosition(ctx, uri, sym.SelectionRange.Start.Line, sym.SelectionRange.Start.Character, false, name)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d matches for %q:\n", len(matches), name)
	for _, sym := range matches {
		fmt.Fprintf(&sb, "\n## %s (%s) line %d\n\n", sym.Name, symbolKindName(sym.Kind), sym.SelectionRange.Start.Line+1)
		result, err := t.executeByPosition(ctx, uri, sym.SelectionRange.Start.Line, sym.SelectionRange.Start.Character, false, name)
		if err != nil {
			fmt.Fprintf(&sb, "(error: %v)\n", err)
			continue
		}
		sb.WriteString(result)
	}
	return sb.String(), nil
}

// snapExplain recovers from a raw position that missed an identifier by
// resolving the enclosing document symbol and re-querying hover once at its
// SelectionRange.Start. When nothing encloses the line it returns an
// actionable error naming nearby symbols. The retry passes allowSnap=false so
// a snap can never recurse.
func (t *ExplainSymbol) snapExplain(ctx context.Context, uri string, line, character uint32) (string, error) {
	snapped, syms, ok := snapPosition(ctx, t.client, uri, line)
	if !ok {
		return "", positionMissErr("explain_symbol", uri, line, syms)
	}
	out, err := t.executeByPosition(ctx, uri, snapped.Line, snapped.Character, false, "")
	if err != nil {
		return "", err
	}
	return snapNotice(uri, line, character, snapped.Line) + out, nil
}

// executeByPosition queries the server at line/character. symbolName is
// non-empty only when plumb resolved that position from a symbol_name
// argument, which selects the failure hint (see queryErr).
func (t *ExplainSymbol) executeByPosition(ctx context.Context, uri string, line, character uint32, allowSnap bool, symbolName string) (string, error) {
	key := fmt.Sprintf("%s:hover:%d:%d", uri, line, character)
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			return v.(string), nil
		}
	}

	hover, err := retryOnServerNotReady(ctx, func() (*protocol.Hover, error) {
		return t.client.Hover(ctx, protocol.HoverParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			Position:     protocol.Position{Line: line, Character: character},
		})
	})
	if err != nil {
		if werr := coldLSPWarmingErr("explain_symbol", t.warmup, uri); werr != nil {
			return "", werr
		}
		if allowSnap && isPositionMissErr(err) {
			return t.snapExplain(ctx, uri, line, character)
		}
		return "", queryErr("explain_symbol", symbolName, err)
	}

	var result string
	if hover == nil || hover.Contents.Value == "" {
		result = fmt.Sprintf("No documentation found for symbol at %s:%d:%d.",
			uri, line+1, character+1)
	} else {
		result = hover.Contents.Value
	}

	if t.cache != nil {
		t.cache.Set(key, result, t.ttl)
	}
	return result, nil
}
