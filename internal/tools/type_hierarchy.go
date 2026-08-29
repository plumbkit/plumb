package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

var typeHierarchySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path containing the type"
    },
    "line": {
      "type": "integer",
      "description": "Zero-based line number of the type. Required when symbol_name is not provided."
    },
    "character": {
      "type": "integer",
      "description": "Zero-based character offset within the line. Required when symbol_name is not provided."
    },
    "symbol_name": {
      "type": "string",
      "description": "Symbol name to look up instead of a position — PREFERRED over line/character. Accepts plain name or ReceiverType.MethodName form. plumb resolves it against the file's symbols, avoiding the off-by-one and 'no identifier found' errors of a hand-computed position. When provided, line and character are not needed."
    },
    "direction": {
      "type": "string",
      "enum": ["supertypes", "subtypes", "both"],
      "description": "Which direction to traverse: parent types (supertypes), child types (subtypes), or both. Defaults to both."
    }
  },
  "required": ["uri"],
  "additionalProperties": false
}`)

// TypeHierarchy implements the type_hierarchy MCP tool.
type TypeHierarchy struct {
	client  lsp.Client
	timeout time.Duration
	warmup  LSPWarmupFn // optional; rewrites a cold-LSP failure into a still-warming advisory
	ws      WorkspaceFn // may be nil; anchors a workspace-relative uri to the pinned root
}

// NewTypeHierarchy creates a TypeHierarchy tool.
func NewTypeHierarchy(client lsp.Client, timeout time.Duration) *TypeHierarchy {
	return &TypeHierarchy{client: client, timeout: timeout}
}

// WithLSPWarmup wires the warm-up probe so a failure against a still-warming
// server says so (and names what answers now) instead of showing the 0-based
// coordinate hint, which would mislead there. Nil-safe.
func (t *TypeHierarchy) WithLSPWarmup(fn LSPWarmupFn) *TypeHierarchy {
	t.warmup = fn
	return t
}

// WithWorkspace anchors a relative uri to the pinned workspace root. Nil-safe.
func (t *TypeHierarchy) WithWorkspace(ws WorkspaceFn) *TypeHierarchy {
	t.ws = ws
	return t
}

func (t *TypeHierarchy) Name() string                 { return "type_hierarchy" }
func (t *TypeHierarchy) InputSchema() json.RawMessage { return typeHierarchySchema }
func (t *TypeHierarchy) Description() string {
	return "Show the type hierarchy for a type: its supertypes (interfaces it implements, embedded types) and subtypes (types that implement or embed it). " +
		"PREFER a name (uri + symbol_name) — plumb resolves the exact identifier position for you, " +
		"avoiding off-by-one errors; a raw file position (uri + line + character) is the fallback " +
		"and, when it lands off an identifier, is snapped to the enclosing symbol. " +
		"Useful for understanding inheritance and polymorphism."
}

type typeHierarchyArgs struct {
	URI        string  `json:"uri"`
	Line       *uint32 `json:"line"`
	Character  *uint32 `json:"character"`
	SymbolName string  `json:"symbol_name"`
	Direction  string  `json:"direction,omitempty"`
}

func parseTypeHierarchyArgs(raw json.RawMessage) (typeHierarchyArgs, error) {
	var a typeHierarchyArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("type_hierarchy: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return a, errors.New("type_hierarchy: uri must not be empty")
	}
	if a.Direction == "" {
		a.Direction = "both"
	}
	return a, nil
}

// typeHierarchyQuery is a request resolved to a concrete cursor position — from
// a raw line/character, a resolved symbol_name, or a snap — so the internal
// query/snap helpers never re-derive a position.
type typeHierarchyQuery struct {
	uri       string
	line      uint32
	character uint32
	direction string
	// symbolName is set only when plumb resolved line/character from a
	// symbol_name argument, so a server rejection is explained as a stale
	// symbol tree rather than as bad coordinates the caller never passed.
	symbolName string
}

// Execute's shape is intentionally near-identical to CallHierarchy.Execute
// (and, modulo the direction param, GetDefinition/ExplainSymbol.Execute) —
// see the comment on GetDefinition.Execute in get_definition.go for why.
//
//nolint:dupl // structurally identical by design across the four query tools, see get_definition.go
func (t *TypeHierarchy) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	a, err := parseTypeHierarchyArgs(args)
	if err != nil {
		return "", err
	}
	return executeLSPQuery(ctx, "type_hierarchy", t.ws, t.timeout, a.URI, a.SymbolName, a.Line, a.Character,
		func(ctx context.Context, uri string) (string, error) {
			return t.executeByName(ctx, uri, a.SymbolName, a.Direction)
		},
		func(ctx context.Context, uri string, line, character uint32) (string, error) {
			q := typeHierarchyQuery{uri: uri, line: line, character: character, direction: a.Direction}
			return t.executeByPosition(ctx, q, true)
		},
	)
}

// executeByName resolves name against the file's document symbols and queries
// the type hierarchy at the identifier position (SelectionRange.Start) — the
// off-by-one-proof path shared with get_definition/find_references/
// call_hierarchy. A name matching several symbols renders every match in turn
// rather than erroring or guessing (never auto-pick among ambiguous
// candidates).
func (t *TypeHierarchy) executeByName(ctx context.Context, uri, name, direction string) (string, error) {
	syms, err := t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		if werr := coldLSPWarmingErr("type_hierarchy", t.warmup, uri); werr != nil {
			return "", werr
		}
		return "", lspTimeoutErr("type_hierarchy", t.timeout, fmt.Errorf("resolving symbol %q: %w", name, err))
	}
	matches := resolveSymbolsByName(syms, name)
	if len(matches) == 0 {
		return fmt.Sprintf("No symbol named %q in %s.%s", name, uri, didYouMean(suggestSymbols(syms, name))), nil
	}
	if len(matches) == 1 {
		return t.executeByPosition(ctx, queryForTypeHierarchy(uri, matches[0], direction, name), false)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d matches for %q:\n", len(matches), name)
	for _, sym := range matches {
		fmt.Fprintf(&sb, "\n## %s (%s) line %d\n\n", sym.Name, symbolKindName(sym.Kind), sym.SelectionRange.Start.Line+1)
		result, err := t.executeByPosition(ctx, queryForTypeHierarchy(uri, sym, direction, name), false)
		if err != nil {
			fmt.Fprintf(&sb, "(error: %v)\n", err)
			continue
		}
		sb.WriteString(result)
	}
	return sb.String(), nil
}

// queryForTypeHierarchy builds a query at a resolved symbol's identifier
// position. name is the symbol_name the caller passed, recorded so a failure
// is reported as a name resolution rather than as a coordinate the caller
// chose.
func queryForTypeHierarchy(uri string, sym protocol.DocumentSymbol, direction, name string) typeHierarchyQuery {
	return typeHierarchyQuery{
		uri:        uri,
		line:       sym.SelectionRange.Start.Line,
		character:  sym.SelectionRange.Start.Character,
		direction:  direction,
		symbolName: name,
	}
}

// snapAndRetry recovers from a raw position that missed an identifier: it
// resolves the enclosing document symbol and re-queries once at its
// SelectionRange.Start. When nothing encloses the line it returns an
// actionable error naming nearby symbols. The retry passes allowSnap=false so
// a snap can never recurse.
func (t *TypeHierarchy) snapAndRetry(ctx context.Context, q typeHierarchyQuery) (string, error) {
	snapped, syms, ok := snapPosition(ctx, t.client, q.uri, q.line)
	if !ok {
		return "", positionMissErr("type_hierarchy", q.uri, q.line, syms)
	}
	snappedQ := q
	snappedQ.line = snapped.Line
	snappedQ.character = snapped.Character
	out, err := t.executeByPosition(ctx, snappedQ, false)
	if err != nil {
		return "", err
	}
	return snapNotice(q.uri, q.line, q.character, snapped.Line) + out, nil
}

func (t *TypeHierarchy) executeByPosition(ctx context.Context, q typeHierarchyQuery, allowSnap bool) (string, error) {
	items, err := retryOnServerNotReady(ctx, func() ([]protocol.TypeHierarchyItem, error) {
		return t.client.PrepareTypeHierarchy(ctx, protocol.PrepareTypeHierarchyParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: q.uri},
			Position:     protocol.Position{Line: q.line, Character: q.character},
		})
	})
	if err != nil {
		if werr := coldLSPWarmingErr("type_hierarchy", t.warmup, q.uri); werr != nil {
			return "", werr
		}
		if allowSnap && isPositionMissErr(err) {
			return t.snapAndRetry(ctx, q)
		}
		return "", queryErr("type_hierarchy", q.symbolName, err)
	}
	if len(items) == 0 {
		return "No type hierarchy item found at the given position." +
			coldLSPEmptyNote(t.warmup, q.uri), nil
	}

	item := items[0]
	var sb strings.Builder
	fmt.Fprintf(&sb, "Type hierarchy for %s (%s) at %s:%d\n\n",
		item.Name, symbolKindName(item.Kind), item.URI, item.Range.Start.Line+1)

	if q.direction == "supertypes" || q.direction == "both" {
		if err := t.renderSupertypes(ctx, &sb, item); err != nil {
			return "", err
		}
	}
	if q.direction == "subtypes" || q.direction == "both" {
		if err := t.renderSubtypes(ctx, &sb, item); err != nil {
			return "", err
		}
	}
	return sb.String(), nil
}

// renderSupertypes appends the "## Supertypes" section for item to sb.
func (t *TypeHierarchy) renderSupertypes(ctx context.Context, sb *strings.Builder, item protocol.TypeHierarchyItem) error {
	supers, err := t.client.Supertypes(ctx, protocol.TypeHierarchySupertypesParams{Item: item})
	if err != nil {
		return lspTimeoutErr("type_hierarchy", t.timeout, fmt.Errorf("supertypes: %w", err))
	}
	sb.WriteString("## Supertypes\n\n")
	writeTypeHierarchyRefs(sb, supers)
	sb.WriteString("\n")
	return nil
}

// renderSubtypes appends the "## Subtypes" section for item to sb.
func (t *TypeHierarchy) renderSubtypes(ctx context.Context, sb *strings.Builder, item protocol.TypeHierarchyItem) error {
	subs, err := t.client.Subtypes(ctx, protocol.TypeHierarchySubtypesParams{Item: item})
	if err != nil {
		return lspTimeoutErr("type_hierarchy", t.timeout, fmt.Errorf("subtypes: %w", err))
	}
	sb.WriteString("## Subtypes\n\n")
	writeTypeHierarchyRefs(sb, subs)
	return nil
}

// writeTypeHierarchyRefs renders a list of type-hierarchy items, or "(none)"
// when empty — shared by renderSupertypes and renderSubtypes.
func writeTypeHierarchyRefs(sb *strings.Builder, items []protocol.TypeHierarchyItem) {
	if len(items) == 0 {
		sb.WriteString("  (none)\n")
		return
	}
	for _, s := range items {
		fmt.Fprintf(sb, "- %s (%s) at %s:%d\n",
			s.Name, symbolKindName(s.Kind), s.URI, s.Range.Start.Line+1)
	}
}
