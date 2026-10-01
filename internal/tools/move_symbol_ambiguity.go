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
// a gopls flat method, which the topology index resolves by receiver (gopls
// itself never nests a method under its type), and "Parent/Name" for a nested
// symbol. nil when any match has none — a generic hint beats a partial list.
func moveNamePaths(syms, matches []protocol.DocumentSymbol) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if recv, method, ok := goMethodReceiver(m.Name); ok {
			out = append(out, recv+"/"+method)
			continue
		}
		parent, ok := enclosingSymbolName(syms, m)
		if !ok {
			return nil
		}
		out = append(out, parent+"/"+m.Name)
	}
	return out
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
