package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

var topologyImpactSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "Symbol name or qualified name to analyse. Must exist in the topology index."
    },
    "depth": {
      "type": "integer",
      "description": "BFS depth for both traversals. Default 3, max 4.",
      "default": 3
    },
    "max_nodes": {
      "type": "integer",
      "description": "Maximum neighbour nodes per direction. Default 100, max 200.",
      "default": 100
    },
    "max_bytes": {
      "type": "integer",
      "description": "Approximate byte budget per direction. Default 30000, max 100000.",
      "default": 30000
    },
    "edge_kinds": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional filter on edge kinds: calls, imports, contains, defines, inherits, implements. Defaults to imports, calls.",
      "default": ["imports","calls"]
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

// TopologyImpact performs a bidirectional BFS to assess blast radius around a symbol.
//
// Concurrency: Execute is safe for concurrent use.
type TopologyImpact struct {
	storeFn func() *topology.Store
}

// NewTopologyImpact returns a new TopologyImpact tool.
func NewTopologyImpact(storeFn func() *topology.Store) *TopologyImpact {
	return &TopologyImpact{storeFn: storeFn}
}

func (*TopologyImpact) Name() string                 { return "topology_impact" }
func (*TopologyImpact) InputSchema() json.RawMessage { return topologyImpactSchema }
func (*TopologyImpact) Description() string {
	return "Bidirectional BFS blast-radius analysis around a named symbol. " +
		"Returns two sections: 'depends on' (outward — what the symbol depends on) and " +
		"'depended on by' (inward — what depends on this symbol). " +
		"Primary use: assess blast radius before a refactor. Source is 'topology' (approximate). " +
		"Returns a clear message when topology is disabled or the symbol is not in the index."
}

type topologyImpactArgs struct {
	Name      string   `json:"name"`
	Depth     int      `json:"depth"`
	MaxNodes  int      `json:"max_nodes"`
	MaxBytes  int      `json:"max_bytes"`
	EdgeKinds []string `json:"edge_kinds"`
	Path      string   `json:"path"`
	Kind      string   `json:"kind"`
}

func (t *TopologyImpact) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseTopologyImpactArgs(raw)
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
	result, alts, runErr := t.run(ctx, store, a)
	if runErr != nil {
		return "", runErr
	}
	return formatImpactResult(result, a, alts), nil
}

func parseTopologyImpactArgs(raw json.RawMessage) (topologyImpactArgs, error) {
	var a topologyImpactArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("topology_impact: invalid arguments: %w", err)
	}
	if a.Depth <= 0 {
		a.Depth = 3
	}
	if a.MaxNodes <= 0 {
		a.MaxNodes = 100
	}
	if a.MaxBytes <= 0 {
		a.MaxBytes = 30000
	}
	if len(a.EdgeKinds) == 0 {
		a.EdgeKinds = []string{"imports", "calls"}
	}
	return a, nil
}

func (a *topologyImpactArgs) validate() error {
	if a.Name == "" {
		return fmt.Errorf("topology_impact: name is required")
	}
	return nil
}

func (t *TopologyImpact) run(ctx context.Context, store *topology.Store, a topologyImpactArgs) (*topology.ImpactResult, []topology.Node, error) {
	if store == nil {
		return nil, nil, nil
	}
	cands, err := store.ResolveNodes(ctx, a.Name, topology.NodeHint{PathSubstr: a.Path, Kind: a.Kind})
	if err != nil {
		return nil, nil, err
	}
	if len(cands) == 0 {
		return nil, nil, fmt.Errorf("topology: symbol %q not found in index", a.Name)
	}
	opts := topology.ImpactOpts{
		Depth:     a.Depth,
		MaxNodes:  a.MaxNodes,
		MaxBytes:  a.MaxBytes,
		EdgeKinds: a.EdgeKinds,
	}
	result, err := store.ImpactFrom(ctx, cands[0], opts)
	if err != nil {
		return nil, nil, err
	}
	return result, cands[1:], nil
}

func formatImpactResult(result *topology.ImpactResult, a topologyImpactArgs, alts []topology.Node) string {
	if result == nil {
		return fmt.Sprintf("topology_impact: symbol %q not found in the index", a.Name)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "topology impact: %s %q (source=topology, depth=%d, edge_kinds=%v)\n",
		string(result.Centre.Kind), result.Centre.Name, a.Depth, a.EdgeKinds)
	fmt.Fprintf(&sb, "  path: %s", result.Centre.Path)
	if result.Centre.StartLine > 0 {
		fmt.Fprintf(&sb, " L%d", result.Centre.StartLine)
	}
	sb.WriteString("\n\n")

	writeImpactSection(&sb, "depends on (outward)", result.DependsOn)
	sb.WriteString("\n")
	writeImpactSection(&sb, "depended on by (inward)", result.DependedOnBy)

	return strings.TrimRight(sb.String(), "\n") + topologyAmbiguityNote(a.Name, alts)
}

func writeImpactSection(sb *strings.Builder, label string, nb *topology.Neighbourhood) {
	if nb == nil || len(nb.Nodes) == 0 {
		fmt.Fprintf(sb, "%s: (none)\n", label)
		return
	}
	fmt.Fprintf(sb, "%s (%d nodes):\n", label, len(nb.Nodes))
	for _, n := range nb.Nodes {
		fmt.Fprintf(sb, "  %s %s — %s", string(n.Kind), n.Name, n.Path)
		if n.StartLine > 0 {
			fmt.Fprintf(sb, " L%d", n.StartLine)
		}
		sb.WriteString("\n")
	}
	if nb.Truncated {
		sb.WriteString("  [truncated]\n")
	}
}
