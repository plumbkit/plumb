// Package-shared symbol-name resolution: the matching rules every LSP-backed
// tool uses to turn a caller-supplied name into documentSymbol nodes, plus the
// kind labels they render. Formerly the guts of the find_symbol tool, which was
// merged into workspace_symbols (see internal/mcp/toolalias.go).
package tools

import (
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// baseSymbolName strips a trailing argument list from a documentSymbol name so a
// plain query ("show") matches a server that names members with their signature
// — sourcekit-lsp reports Swift methods as "show()" / "show(animated:)". Returns
// the name unchanged when there is no "(".
func baseSymbolName(name string) string {
	if before, _, ok := strings.Cut(name, "("); ok {
		return strings.TrimSpace(before)
	}
	return name
}

// symbolNameMatches reports whether a documentSymbol name matches a query,
// either exactly or after stripping a trailing argument list (Swift members).
func symbolNameMatches(symName, query string) bool {
	return symName == query || baseSymbolName(symName) == query
}

// resolveSymbolsByName returns all symbols in the tree matching name.
//
// For a dotted "ReceiverType.MethodName" it matches two shapes: the nested
// shape, where the method is a child of a type symbol (Python, Java, and the
// tree-sitter extractors), and the flat shape, where the method is a top-level
// symbol named "(*Recv).Method" or "(Recv).Method" (gopls' Go output — methods
// are never nested under the receiver type). For plain names it matches at any
// depth.
func resolveSymbolsByName(syms []protocol.DocumentSymbol, name string) []protocol.DocumentSymbol {
	if parent, child, ok := strings.Cut(name, "."); ok {
		parentType := goReceiverType(parent)
		var out []protocol.DocumentSymbol
		for _, s := range syms {
			if symbolNameMatches(s.Name, parent) {
				for _, c := range s.Children {
					if symbolNameMatches(c.Name, child) {
						out = append(out, c)
					}
				}
			}
			if recv, method, ok := goMethodReceiver(s.Name); ok && recv == parentType && method == child {
				out = append(out, s)
			}
		}
		return out
	}
	var out []protocol.DocumentSymbol
	var walk func([]protocol.DocumentSymbol)
	walk = func(ss []protocol.DocumentSymbol) {
		for _, s := range ss {
			if symbolNameMatches(s.Name, name) {
				out = append(out, s)
			}
			walk(s.Children)
		}
	}
	walk(syms)
	return out
}

// goReceiverType strips Go receiver decoration so a dotted-name parent of
// "(*Foo)", "*Foo", or "Foo" all normalise to "Foo".
func goReceiverType(parent string) string {
	return strings.TrimPrefix(strings.Trim(parent, "()"), "*")
}

// goMethodReceiver splits a gopls Go method symbol name — "(*Recv).Method" or
// "(Recv).Method" — into its receiver type and method. ok is false for any name
// not in that form (plain functions, types, fields).
func goMethodReceiver(symName string) (recv, method string, ok bool) {
	if !strings.HasPrefix(symName, "(") {
		return "", "", false
	}
	i := strings.Index(symName, ").")
	if i < 0 {
		return "", "", false
	}
	recv = strings.TrimPrefix(symName[1:i], "*")
	method = symName[i+2:]
	if recv == "" || method == "" || strings.Contains(method, ".") {
		return "", "", false
	}
	return recv, method, true
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

// disambiguatedCandidate is one match in an ambiguous name-resolution result,
// rendered for a caller who needs to retry. SymbolName is a value PROVEN — by
// actually calling resolveSymbolsByName, not merely asserted — to resolve
// back to exactly this match; it is "" when no such value exists, and the
// caller must fall back to Line/Character (0-based, as everywhere else in
// this package) instead of inventing a symbol_name that would only re-error.
// See provenSymbolName for why the proof matters (PLAN-363 review round 1: an
// earlier version of this type emitted plausible-looking but unresolvable
// names).
type disambiguatedCandidate struct {
	Name       string
	Line       uint32
	Character  uint32
	SymbolName string // "" when no symbol_name value is proven to round-trip to this match
}

// disambiguatedNames builds one disambiguatedCandidate per match in an
// ambiguous name-resolution result, so a caller who hit "N symbols named X"
// can retry with an exact, WORKING value instead of guessing coordinates —
// see provenSymbolName for the round-trip guarantee.
func disambiguatedNames(syms []protocol.DocumentSymbol, matches []protocol.DocumentSymbol) []disambiguatedCandidate {
	out := make([]disambiguatedCandidate, 0, len(matches))
	for _, m := range matches {
		c := disambiguatedCandidate{
			Name:      m.Name,
			Line:      m.SelectionRange.Start.Line,
			Character: m.SelectionRange.Start.Character,
		}
		if name, ok := provenSymbolName(syms, m); ok {
			c.SymbolName = name
		}
		out = append(out, c)
	}
	return out
}

// provenSymbolName returns a symbol_name value that resolveSymbolsByName is
// PROVEN — by actually calling it, the same resolver a retry will call — to
// resolve back to exactly target and nothing else. Candidates are tried in
// preference order: the Go flat "(*Recv).Method"/"(Recv).Method" form gopls
// already reports, normalised to "Recv.Method"; then the nearest enclosing
// symbol's name joined as "Parent.Child" (Python/Java-style nesting). A
// candidate is used only when re-resolving it yields EXACTLY ONE match that
// IS target (same Name and SelectionRange.Start) — never a name that merely
// looks plausible. This catches, by construction, every reason a candidate
// can fail to round-trip: resolveSymbolsByName's dotted resolver only scans
// TOP-LEVEL parents (so a "Parent.Child" built from a deeper-nested match
// would not resolve), and a dotted query can simultaneously match a nested
// AND a flat-form symbol (so the "obvious" name can still be ambiguous).
// ok is false — never a fabricated string — when nothing proves out; the
// caller must fall back to target's raw line/character.
func provenSymbolName(syms []protocol.DocumentSymbol, target protocol.DocumentSymbol) (string, bool) {
	var candidates []string
	if recv, method, ok := goMethodReceiver(target.Name); ok {
		candidates = append(candidates, recv+"."+method)
	}
	if parent, ok := enclosingSymbolName(syms, target); ok {
		candidates = append(candidates, parent+"."+target.Name)
	}
	for _, cand := range candidates {
		resolved := resolveSymbolsByName(syms, cand)
		if len(resolved) == 1 && sameSymbol(resolved[0], target) {
			return cand, true
		}
	}
	return "", false
}

// sameSymbol reports whether a and b are the same document symbol, identified
// by Name plus SelectionRange.Start (unique per symbol in one document-symbol
// response).
func sameSymbol(a, b protocol.DocumentSymbol) bool {
	return a.Name == b.Name && a.SelectionRange.Start == b.SelectionRange.Start
}

// enclosingSymbolName finds target's immediate parent anywhere in the tree,
// identified by its SelectionRange.Start plus Name (unique per symbol in one
// document-symbol response), as a CANDIDATE for a "Parent.Child" disambiguated
// name — provenSymbolName is what actually verifies the candidate resolves
// back to target before it is ever offered to a caller, so this need not (and
// deliberately does not) limit itself to depth-1 nesting.
func enclosingSymbolName(syms []protocol.DocumentSymbol, target protocol.DocumentSymbol) (string, bool) {
	for _, s := range syms {
		for _, c := range s.Children {
			if c.SelectionRange.Start == target.SelectionRange.Start && c.Name == target.Name {
				return s.Name, true
			}
		}
		if name, ok := enclosingSymbolName(s.Children, target); ok {
			return name, ok
		}
	}
	return "", false
}

// formatDisambiguation renders an ambiguous-name candidate list into
// copy-pasteable retry guidance: a symbol_name for every candidate with one
// PROVEN to round-trip (disambiguatedCandidate.SymbolName != ""), and an
// explicit line/character fallback for any that don't — never a symbol_name
// string that would just re-error.
func formatDisambiguation(cands []disambiguatedCandidate) string {
	parts := make([]string, 0, len(cands))
	for _, c := range cands {
		if c.SymbolName != "" {
			parts = append(parts, c.SymbolName)
		} else {
			parts = append(parts, fmt.Sprintf("%q at line %d (no unique symbol_name — retry with line:%d character:%d instead)",
				c.Name, c.Line+1, c.Line, c.Character))
		}
	}
	return strings.Join(parts, "; ")
}

func symbolKindName(k protocol.SymbolKind) string {
	if name, ok := symbolKindNames[k]; ok {
		return name
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}
