package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

var getDefinitionSchema = json.RawMessage(`{
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

// GetDefinition returns the definition location(s) for a symbol at a position
// or by name.
type GetDefinition struct {
	client  lsp.Client
	cache   *cache.Cache
	ttl     time.Duration
	timeout time.Duration
	ws      WorkspaceFn     // may be nil; anchors a workspace-relative uri to the pinned root
	topo    topologyStoreFn // may be nil; the by-name topology fallback when the LSP is unavailable
	warmup  LSPWarmupFn     // may be nil; distinguishes a warming server from an unavailable one in the fallback note
}

// NewGetDefinition creates a GetDefinition tool. Pass a nil cache to disable caching.
func NewGetDefinition(client lsp.Client, c *cache.Cache, ttl, timeout time.Duration) *GetDefinition {
	return &GetDefinition{client: client, cache: c, ttl: ttl, timeout: timeout}
}

// WithWorkspace anchors a relative uri to the pinned workspace root. Nil-safe.
func (t *GetDefinition) WithWorkspace(ws WorkspaceFn) *GetDefinition {
	t.ws = ws
	return t
}

// WithTopologyFallback wires the topology store so a by-name lookup degrades to
// the tree-sitter index when the language server is unavailable (still warming,
// or erroring) instead of surfacing the error. Approximate (declaration line,
// not the precise definition), and only for the symbol_name path — a raw
// position has no name to resolve against the index. Nil-safe. Returns the
// receiver for chaining.
func (t *GetDefinition) WithTopologyFallback(fn topologyStoreFn) *GetDefinition {
	t.topo = fn
	return t
}

// WithLSPWarmup wires the warm-up probe so the topology-fallback note says
// "still warming — retry shortly" instead of "language server unavailable"
// while the server's handshake completes. Nil-safe; returns the tool for
// chaining.
func (t *GetDefinition) WithLSPWarmup(fn LSPWarmupFn) *GetDefinition {
	t.warmup = fn
	return t
}

func (t *GetDefinition) Name() string                 { return "get_definition" }
func (t *GetDefinition) InputSchema() json.RawMessage { return getDefinitionSchema }
func (t *GetDefinition) Description() string {
	return "Returns the SOURCE LOCATION (file path + line number) of where a symbol is defined. " +
		"No native Claude Code equivalent for LSP-backed semantic definition lookup. " +
		"PREFER a name (uri + symbol_name) — plumb resolves the exact identifier position " +
		"for you, avoiding off-by-one errors; a raw file position (uri + line + character) " +
		"is the fallback and, when it lands off an identifier, is snapped to the enclosing symbol. " +
		"Use when you need to navigate to the implementation of a symbol. " +
		"For documentation or type signatures at the same position, use explain_symbol instead."
}

type getDefinitionArgs struct {
	URI        string  `json:"uri"`
	Line       *uint32 `json:"line"`
	Character  *uint32 `json:"character"`
	SymbolName string  `json:"symbol_name"`
}

func (t *GetDefinition) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a getDefinitionArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("get_definition: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return "", fmt.Errorf("get_definition: uri must not be empty")
	}
	a.URI = toFileURIAnchored(a.URI, t.ws)

	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()

	if a.SymbolName != "" {
		return t.executeByName(ctx, a.URI, a.SymbolName)
	}

	if a.Line == nil || a.Character == nil {
		return "", fmt.Errorf("get_definition: either symbol_name or both line and character are required")
	}
	return t.executeByPosition(ctx, a.URI, *a.Line, *a.Character, true, "")
}

// executeByName resolves a definition through the language server, falling back
// to the topology index by name when any LSP step fails (the still-warming
// server is the common case). The fallback fires only on a hard error, never on
// an authoritative "no symbol named…" answer, so a working server's negative
// result is never masked by a stale index hit.
func (t *GetDefinition) executeByName(ctx context.Context, uri, name string) (string, error) {
	result, err := t.lspDefinitionByName(ctx, uri, name)
	if err != nil {
		if fb, ok := topologyDefinitionFallback(t.topo, topologyDefinitionNoteFor(t.warmup, uri), name); ok {
			return fb, nil
		}
		return "", err
	}
	return result, nil
}

func (t *GetDefinition) lspDefinitionByName(ctx context.Context, uri, name string) (string, error) {
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
			return "", lspTimeoutErr("get_definition", t.timeout, fmt.Errorf("resolving symbol %q: %w", name, err))
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

// snapDefinition recovers from a raw position that missed an identifier by
// resolving the enclosing document symbol and re-querying the definition once at
// its SelectionRange.Start. When nothing encloses the line it returns an
// actionable error naming nearby symbols. The retry passes allowSnap=false so a
// snap can never recurse.
func (t *GetDefinition) snapDefinition(ctx context.Context, uri string, line, character uint32) (string, error) {
	snapped, syms, ok := snapPosition(ctx, t.client, uri, line)
	if !ok {
		return "", positionMissErr("get_definition", uri, line, syms)
	}
	out, err := t.executeByPosition(ctx, uri, snapped.Line, snapped.Character, false, "")
	if err != nil {
		return "", err
	}
	return snapNotice(uri, line, character, snapped.Line) + out, nil
}

// executeByPosition queries the server at line/character. symbolName is non-empty
// only when plumb resolved that position from a symbol_name argument, which
// selects the failure hint (see queryErr).
func (t *GetDefinition) executeByPosition(ctx context.Context, uri string, line, character uint32, allowSnap bool, symbolName string) (string, error) {
	key := fmt.Sprintf("%s:def:%d:%d", uri, line, character)
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			return v.(string), nil
		}
	}

	locs, err := t.client.Definition(ctx, protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: line, Character: character},
	})
	if err != nil {
		if allowSnap && isPositionMissErr(err) {
			return t.snapDefinition(ctx, uri, line, character)
		}
		return "", queryErr("get_definition", symbolName, err)
	}

	var result string
	if len(locs) == 0 {
		result = fmt.Sprintf("No definition found for symbol at %s:%d:%d.",
			uri, line+1, character+1)
	} else {
		var sb strings.Builder
		if len(locs) == 1 {
			l := locs[0]
			fmt.Fprintf(&sb, "Definition at %s:%d:%d\n",
				l.URI, l.Range.Start.Line+1, l.Range.Start.Character+1)
		} else {
			fmt.Fprintf(&sb, "%d definitions for symbol at %s:%d:%d:\n\n",
				len(locs), uri, line+1, character+1)
			for i, l := range locs {
				fmt.Fprintf(&sb, "%d. %s:%d:%d\n",
					i+1, l.URI, l.Range.Start.Line+1, l.Range.Start.Character+1)
			}
		}
		result = sb.String()
	}

	if t.cache != nil {
		t.cache.Set(key, result, t.ttl)
	}
	return result, nil
}
