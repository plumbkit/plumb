package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// isPositionMissErr reports whether err is a language-server rejection of a
// cursor position that pointed at no identifier — a blank line, whitespace, a
// comment, or a column past the line's end. Matched narrowly against the
// observed gopls messages so a snap-and-retry never swallows an unrelated
// failure (a timeout, a missing snapshot, a genuine server error): each of
// these means "the position is fine syntactically, it just isn't on a symbol".
// gopls phrases it "no identifier found" for definition/references but
// "identifier not found" for prepareCallHierarchy, so both are matched.
func isPositionMissErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no identifier found") ||
		strings.Contains(msg, "identifier not found") ||
		strings.Contains(msg, "column is beyond end of line")
}

// snapPosition resolves the position to re-query after a raw line/character
// missed an identifier: the SelectionRange.Start (the identifier itself, not the
// declaration keyword) of the smallest document symbol whose range encloses
// line. Using SelectionRange.Start makes a snapped query byte-for-byte identical
// to the off-by-one-proof symbol_name path. ok is false when the server returns
// no symbols or none enclose the line; syms is returned regardless so the caller
// can build an actionable "did you mean" error without a second round-trip.
func snapPosition(ctx context.Context, client lsp.Client, uri string, line uint32) (pos protocol.Position, syms []protocol.DocumentSymbol, ok bool) {
	syms, err := client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil || len(syms) == 0 {
		return protocol.Position{}, syms, false
	}
	enc := deepestEnclosingDocSymbol(syms, line)
	if enc == nil {
		return protocol.Position{}, syms, false
	}
	return enc.SelectionRange.Start, syms, true
}

// snapNotice prefixes a result answered after snapping a missed raw position to
// its enclosing symbol, so the caller can see the query was resolved for a
// nearby symbol rather than the exact coordinates it asked for, and is nudged
// toward symbol_name for an exact query.
func snapNotice(uri string, line, character, snappedLine uint32) string {
	return fmt.Sprintf("note: no identifier at %s:%d:%d; answered for the enclosing "+
		"symbol at line %d — pass symbol_name for an exact query.\n\n",
		uri, line+1, character+1, snappedLine+1)
}

// positionMissErr builds an actionable error for a raw position that resolved to
// no identifier and could not be snapped to an enclosing symbol. It names the
// symbols nearest the requested line so the caller can re-issue with symbol_name
// instead of guessing coordinates — replacing the raw 0-based coordinate hint.
func positionMissErr(tool, uri string, line uint32, syms []protocol.DocumentSymbol) error {
	if hint := nearbySymbolHint(syms, line); hint != "" {
		return fmt.Errorf("%s: no identifier at %s:%d; %s — pass it as symbol_name",
			tool, uri, line+1, hint)
	}
	return fmt.Errorf("%s: no identifier at %s:%d — the position is not on a symbol; "+
		"pass symbol_name (uri + symbol_name) instead of line/character",
		tool, uri, line+1)
}

// nearbySymbolHint returns a "did you mean <Sym> at line M?" fragment naming the
// up-to-three document symbols whose declaration line is closest to line,
// nearest first, or "" when there are no symbols.
func nearbySymbolHint(syms []protocol.DocumentSymbol, line uint32) string {
	flat := flattenDocSymbols(syms)
	if len(flat) == 0 {
		return ""
	}
	sort.SliceStable(flat, func(i, j int) bool {
		return lineDistance(flat[i], line) < lineDistance(flat[j], line)
	})
	n := min(3, len(flat))
	parts := make([]string, 0, n)
	for _, s := range flat[:n] {
		parts = append(parts, fmt.Sprintf("%s at line %d", s.Name, s.SelectionRange.Start.Line+1))
	}
	return "did you mean " + strings.Join(parts, ", ") + "?"
}

// flattenDocSymbols returns every symbol in the tree in a single slice
// (parents and their descendants), so nearby-symbol ranking can consider nested
// methods as well as top-level declarations.
func flattenDocSymbols(syms []protocol.DocumentSymbol) []protocol.DocumentSymbol {
	var out []protocol.DocumentSymbol
	var walk func([]protocol.DocumentSymbol)
	walk = func(ss []protocol.DocumentSymbol) {
		for i := range ss {
			out = append(out, ss[i])
			walk(ss[i].Children)
		}
	}
	walk(syms)
	return out
}

// lineDistance is the absolute distance in lines between a symbol's declaration
// (SelectionRange.Start) and line, used to rank "did you mean" candidates.
func lineDistance(s protocol.DocumentSymbol, line uint32) int {
	d := int(s.SelectionRange.Start.Line) - int(line)
	if d < 0 {
		return -d
	}
	return d
}
