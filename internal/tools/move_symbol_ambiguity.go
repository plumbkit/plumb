package tools

// move_symbol_ambiguity.go — what move_symbol says when a bare name_path
// names more than one declaration, split from move_symbol.go for its size cap.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// moveNamePaths returns a name_path for each ambiguous match: "Recv/Method" for
// a gopls flat method (gopls itself never nests a method under its type), with
// the receiver's type parameters stripped — "(*S[T]).Run" is offered as "S/Run",
// the receiver both findSymbolByPath and the topology index resolve — and
// "Parent/Name" for a nested symbol.
//
// A path is offered only once findSymbolByPath, the resolver a retry calls first,
// is seen to return exactly that match. A symbol nested two deep names its
// parent, yet findSymbolByPath follows a path from the top level, so a hint like
// "Mid/Run" would come back "not found". nil when any match has no such path —
// a generic hint beats a partial list, and a path that errors is no hint at all.
func moveNamePaths(syms, matches []protocol.DocumentSymbol) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		path, ok := moveNamePath(syms, m)
		if !ok {
			return nil
		}
		if got := findSymbolByPath(syms, path); got == nil || !sameSymbol(*got, m) {
			return nil
		}
		out = append(out, path)
	}
	return out
}

// moveNamePath is the candidate name_path for one match, before moveNamePaths
// proves it.
func moveNamePath(syms []protocol.DocumentSymbol, m protocol.DocumentSymbol) (string, bool) {
	if recv, method, ok := goMethodReceiver(m.Name); ok {
		return stripTypeParams(recv) + "/" + method, true
	}
	parent, ok := enclosingSymbolName(syms, m)
	if !ok {
		return "", false
	}
	return parent + "/" + m.Name, true
}

// moveAmbiguousErr is the single refusal both ambiguity checks return, so the
// message an agent reads does not depend on which tree answered. namePaths, when
// every match has one, are the exact name_paths that single each out.
func moveAmbiguousErr(n int, namePath, uri string, namePaths []string) error {
	remedy := "disambiguate with a slash-separated name_path (Parent/Name)"
	if len(namePaths) > 0 {
		quoted := make([]string, len(namePaths))
		for i, p := range namePaths {
			quoted[i] = strconv.Quote(p)
		}
		remedy = "pass one of these name_paths: " + strings.Join(quoted, ", ")
	}
	return fmt.Errorf("move_symbol: %d symbols named %q in %s — ambiguous; v1 moves one declaration, %s",
		n, namePath, paths.URIToPath(uri), remedy)
}
