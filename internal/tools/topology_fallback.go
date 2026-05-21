package tools

import (
	"fmt"
	"strings"

	"github.com/golimpio/plumb/internal/topology"
)

// topologyStoreFn returns the active topology store, or nil when topology is
// disabled or the workspace is not yet attached. Mirrors the topoFn accessor
// the dedicated topology_* tools already use.
type topologyStoreFn = func() *topology.Store

// topologyFallbackNote prefixes every fallback response so the caller knows the
// answer came from the (possibly stale, heuristic) topology index rather than a
// live language server.
const topologyFallbackNote = "[topology fallback — LSP unavailable; results are approximate and may be stale. source=topology, mode=indexed-approximate]"

// activeTopology resolves the store from a nil-safe accessor.
func activeTopology(fn topologyStoreFn) *topology.Store {
	if fn == nil {
		return nil
	}
	return fn()
}

// filterTopologyByName returns nodes whose name contains query (case-insensitive),
// mirroring the substring matching of find_symbol's LSP path.
func filterTopologyByName(nodes []topology.Node, query string) []topology.Node {
	q := strings.ToLower(query)
	out := make([]topology.Node, 0, len(nodes))
	for _, n := range nodes {
		if strings.Contains(strings.ToLower(n.Name), q) {
			out = append(out, n)
		}
	}
	return out
}

// formatTopologyMatches renders a name-lookup fallback result.
func formatTopologyMatches(header string, nodes []topology.Node) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n%s:\n\n", topologyFallbackNote, header)
	if len(nodes) == 0 {
		sb.WriteString("(no matching symbols in the index)\n")
		return sb.String()
	}
	for _, n := range nodes {
		fmt.Fprintf(&sb, "- %s (%s) at %s:%d\n", n.Name, string(n.Kind), n.Path, n.StartLine)
	}
	return sb.String()
}

// formatTopologyOutline renders a single-file outline fallback result.
func formatTopologyOutline(uri string, nodes []topology.Node) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\nSymbols in %s (%d, source: topology)\n\n", topologyFallbackNote, uri, len(nodes))
	for _, n := range nodes {
		if n.EndLine == 0 || n.StartLine == n.EndLine {
			fmt.Fprintf(&sb, "%s (%s) line %d\n", n.Name, string(n.Kind), n.StartLine)
		} else {
			fmt.Fprintf(&sb, "%s (%s) lines %d–%d\n", n.Name, string(n.Kind), n.StartLine, n.EndLine)
		}
	}
	return sb.String()
}
