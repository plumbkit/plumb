package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/golimpio/plumb/internal/topology"
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
      "description": "How much source detail to include: none, signatures (default), snippets, full.",
      "default": "signatures"
    },
    "edge_kinds": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional filter on edge kinds: calls, imports, contains, defines, inherits, implements."
    }
  },
  "required": ["name"]
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
	nb, runErr := t.run(ctx, store, a)
	if runErr != nil {
		return "", runErr
	}
	return formatTopologyNeighbourhood(nb, a), nil
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

func (t *TopologyExplore) run(ctx context.Context, store *topology.Store, a topologyExploreArgs) (*topology.Neighbourhood, error) {
	if store == nil {
		return nil, fmt.Errorf("topology_explore: topology indexing is disabled for this session — " +
			"set [topology] enabled = true in .plumb/config.toml to enable")
	}
	opts := topology.ExploreOpts{
		Depth:         a.Depth,
		MaxNodes:      a.MaxNodes,
		MaxBytes:      a.MaxBytes,
		IncludeSource: a.IncludeSource,
		EdgeKinds:     a.EdgeKinds,
	}
	return store.Explore(ctx, a.Name, opts)
}

func formatTopologyNeighbourhood(nb *topology.Neighbourhood, a topologyExploreArgs) string {
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
	return strings.TrimRight(sb.String(), "\n")
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
}
