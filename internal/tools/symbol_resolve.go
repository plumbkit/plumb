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
	if i := strings.IndexByte(name, '('); i >= 0 {
		return strings.TrimSpace(name[:i])
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

// disambiguatedNames returns a copy-pasteable symbol_name value for each match
// in an ambiguous name-resolution result, so a caller who hit "N symbols
// named X" can retry with an exact value instead of guessing coordinates.
// Preference order: the Go flat "(*Recv).Method"/"(Recv).Method" form gopls
// already reports, normalised to "Recv.Method"; else the nearest enclosing
// symbol's name joined as "Parent.Child" (Python/Java-style nesting); else a
// line-qualified fallback for a top-level duplicate name resolveSymbolsByName
// cannot otherwise disambiguate.
func disambiguatedNames(syms []protocol.DocumentSymbol, matches []protocol.DocumentSymbol) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if recv, method, ok := goMethodReceiver(m.Name); ok {
			out = append(out, recv+"."+method)
			continue
		}
		if parent, ok := enclosingSymbolName(syms, m); ok {
			out = append(out, parent+"."+m.Name)
			continue
		}
		out = append(out, fmt.Sprintf("%s (line %d)", m.Name, m.SelectionRange.Start.Line+1))
	}
	return out
}

// enclosingSymbolName finds target's immediate parent in the tree, identified
// by its SelectionRange.Start plus Name (unique per symbol in one
// document-symbol response) so a caller can build a "Parent.Child"
// disambiguated name for a nested match that resolveSymbolsByName itself does
// not track parentage for.
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

func symbolKindName(k protocol.SymbolKind) string {
	if name, ok := symbolKindNames[k]; ok {
		return name
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}
