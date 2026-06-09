package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

var topologyExploreSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "Symbol name or qualified name to explore. Must exist in the topology index."
    },
    "depth": {
      "type": "integer",
      "description": "BFS depth from the centre node. Default 2, max 4.",
      "default": 2
    },
    "max_nodes": {
      "type": "integer",
      "description": "Maximum number of neighbour nodes to return. Default 50, max 200.",
      "default": 50
    },
    "max_bytes": {
      "type": "integer",
      "description": "Approximate byte budget for neighbour data. Default 30000, max 100000.",
      "default": 30000
    },
    "include_source": {
      "type": "string",
      "description": "How much source detail to include per symbol: none (name only), signatures (default), or snippets/full (signature plus docstring). Symbols are always returned whole — max_bytes truncates on symbol boundaries, never mid-function.",
      "default": "signatures"
    },
    "edge_kinds": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional filter on edge kinds: calls, imports, contains, defines, inherits, implements."
    },
    "path": {
      "type": "string",
      "description": "Optional file-path substring to disambiguate when several indexed symbols share this name (case-insensitive)."
    },
    "kind": {
      "type": "string",
      "description": "Optional node kind to disambiguate a shared name: function, method, type, class, constant, variable, field, …"
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`)

// TopologyExplore performs a bounded BFS neighbourhood around a named symbol.
//
// Concurrency: Execute is safe for concurrent use.
type TopologyExplore struct {
	storeFn func() *topology.Store
}

// NewTopologyExplore returns a new TopologyExplore tool.
// storeFn returns the current topology.Store for the session, or nil if disabled.
func NewTopologyExplore(storeFn func() *topology.Store) *TopologyExplore {
	return &TopologyExplore{storeFn: storeFn}
}

func (*TopologyExplore) Name() string                 { return "topology_explore" }
func (*TopologyExplore) InputSchema() json.RawMessage { return topologyExploreSchema }
func (*TopologyExplore) Description() string {
	return "Bounded BFS neighbourhood around a named symbol in the topology index. " +
		"Returns the centre node, neighbour nodes, and connecting edges up to depth/max_nodes/max_bytes. " +
		"Reports truncation when limits are hit. Source is 'topology' (approximate — use LSP semantic " +
		"tools for authoritative reference and definition lookups). Returns an error when topology is " +
		"disabled or the symbol is not in the index."
}

type topologyExploreArgs struct {
	Name          string   `json:"name"`
	Depth         int      `json:"depth"`
	MaxNodes      int      `json:"max_nodes"`
	MaxBytes      int      `json:"max_bytes"`
	IncludeSource string   `json:"include_source"`
	EdgeKinds     []string `json:"edge_kinds"`
	Path          string   `json:"path"`
	Kind          string   `json:"kind"`
}

func (t *TopologyExplore) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseTopologyExploreArgs(raw)
	if err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	store := t.storeFn()
	if store == nil {
		return topologyDisabledMessage(), nil
	}
	nb, alts, runErr := t.run(ctx, store, a)
	if runErr != nil {
		return "", runErr
	}
	return formatTopologyNeighbourhood(nb, a, alts), nil
}

func parseTopologyExploreArgs(raw json.RawMessage) (topologyExploreArgs, error) {
	var a topologyExploreArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("topology_explore: invalid arguments: %w", err)
	}
	if a.IncludeSource == "" {
		a.IncludeSource = "signatures"
	}
	return a, nil
}

func (a *topologyExploreArgs) validate() error {
	if a.Name == "" {
		return fmt.Errorf("topology_explore: name is required")
	}
	return nil
}

func (t *TopologyExplore) run(ctx context.Context, store *topology.Store, a topologyExploreArgs) (*topology.Neighbourhood, []topology.Node, error) {
	if store == nil {
		return nil, nil, nil // defensive; Execute pre-checks a nil store
	}
	cands, err := store.ResolveNodes(ctx, a.Name, topology.NodeHint{PathSubstr: a.Path, Kind: a.Kind})
	if err != nil {
		return nil, nil, err
	}
	if len(cands) == 0 {
		return nil, nil, fmt.Errorf("topology: symbol %q not found in index", a.Name)
	}
	opts := topology.ExploreOpts{
		Depth:         a.Depth,
		MaxNodes:      a.MaxNodes,
		MaxBytes:      a.MaxBytes,
		IncludeSource: a.IncludeSource,
		EdgeKinds:     a.EdgeKinds,
	}
	nb, err := store.ExploreFrom(ctx, cands[0], opts)
	if err != nil {
		return nil, nil, err
	}
	return nb, cands[1:], nil
}

func formatTopologyNeighbourhood(nb *topology.Neighbourhood, a topologyExploreArgs, alts []topology.Node) string {
	if nb == nil {
		return fmt.Sprintf("topology_explore: symbol %q not found in the index", a.Name)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "topology explore: %s %q (source=topology)\n", string(nb.Centre.Kind), nb.Centre.Name)
	fmt.Fprintf(&sb, "  path: %s", nb.Centre.Path)
	if nb.Centre.StartLine > 0 {
		fmt.Fprintf(&sb, " L%d", nb.Centre.StartLine)
	}
	sb.WriteString("\n")
	if a.IncludeSource != "none" && nb.Centre.Signature != "" {
		fmt.Fprintf(&sb, "  sig:  %s\n", nb.Centre.Signature)
	}
	if wantsDocstring(a.IncludeSource) && nb.Centre.Docstring != "" {
		fmt.Fprintf(&sb, "  doc:  %s\n", firstLine(nb.Centre.Docstring))
	}
	sb.WriteString("\n")

	if len(nb.Nodes) == 0 {
		sb.WriteString("no neighbours found\n")
	} else {
		fmt.Fprintf(&sb, "neighbours (%d):\n", len(nb.Nodes))
		for _, n := range nb.Nodes {
			writeNeighbourLine(&sb, n, a.IncludeSource)
		}
	}

	if len(nb.Edges) > 0 {
		fmt.Fprintf(&sb, "\nedges (%d):\n", len(nb.Edges))
		for _, e := range nb.Edges {
			fmt.Fprintf(&sb, "  %d -[%s]-> %d (conf=%.2f)\n", e.FromID, string(e.Kind), e.ToID, e.Confidence)
		}
	}

	if nb.Truncated {
		sb.WriteString("\n[truncated: max_nodes or max_bytes reached — reduce depth or increase limits]\n")
	}
	return strings.TrimRight(sb.String(), "\n") + topologyAmbiguityNote(a.Name, alts)
}

// topologyAmbiguityNote returns a trailing note when a symbol name resolved to
// more than one indexed node, listing the alternatives so the agent can re-query
// with a path/kind hint. Returns "" when the name was unambiguous.
func topologyAmbiguityNote(name string, alternatives []topology.Node) string {
	if len(alternatives) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n\n[note: %q matched %d symbols; showing the first. Pass path/kind to disambiguate. Other matches:",
		name, len(alternatives)+1)
	for _, n := range alternatives {
		fmt.Fprintf(&sb, "\n  %s %s — %s", string(n.Kind), n.Name, n.Path)
		if n.StartLine > 0 {
			fmt.Fprintf(&sb, " L%d", n.StartLine)
		}
	}
	sb.WriteString("]")
	return sb.String()
}

func writeNeighbourLine(sb *strings.Builder, n topology.Node, includeSource string) {
	fmt.Fprintf(sb, "  %s %s — %s", string(n.Kind), n.Name, n.Path)
	if n.StartLine > 0 {
		fmt.Fprintf(sb, " L%d", n.StartLine)
	}
	sb.WriteString("\n")
	if includeSource != "none" && n.Signature != "" {
		fmt.Fprintf(sb, "    sig: %s\n", n.Signature)
	}
	if wantsDocstring(includeSource) && n.Docstring != "" {
		fmt.Fprintf(sb, "    doc: %s\n", firstLine(n.Docstring))
	}
}

// wantsDocstring reports whether the source mode includes docstrings (the
// richer "snippets"/"full" modes), as opposed to signatures alone.
func wantsDocstring(includeSource string) bool {
	return includeSource == "snippets" || includeSource == "full"
}

// firstLine returns the first non-empty line of s, trimmed and length-capped,
// so a multi-line docstring contributes one compact line to the output.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			if len(ln) > 120 {
				return ln[:120] + "…"
			}
			return ln
		}
	}
	return ""
}
