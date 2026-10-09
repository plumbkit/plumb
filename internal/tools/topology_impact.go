package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

var topologyImpactSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "Symbol or qualified name in the topology index (not needed for reachability)."
    },
    "depth": {
      "type": "integer",
      "description": "BFS depth both ways (default 3, max 4).",
      "default": 3
    },
    "max_nodes": {
      "type": "integer",
      "description": "Neighbours per direction (default 100, max 200).",
      "default": 100
    },
    "max_bytes": {
      "type": "integer",
      "description": "Byte budget per direction (default 30000, max 100000).",
      "default": 30000
    },
    "edge_kinds": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Edge kinds to follow: calls, imports, contains, defines, inherits, implements (default imports, calls).",
      "default": ["imports","calls"]
    },
    "path": {
      "type": "string",
      "description": "File-path substring to pick among same-named symbols."
    },
    "kind": {
      "type": "string",
      "description": "Node kind to pick among same-named symbols: function, method, type, class, …"
    },
    "mode": {
      "type": "string",
      "description": "\"reachability\" for entry-point reachability (Go only); roots, path_to and layers need it."
    },
    "granularity": {
      "type": "string",
      "enum": ["package", "function"],
      "default": "package",
      "description": "Reachability: package (default; production imports) or function (admitted call graph; test callers excluded, dynamic calls disclosed)."
    },
    "roots": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Reachability roots: package dirs or \"main\" (package); file.go#Symbol or \"main\" (function). Default: main plus topology_routes roots."
    },
    "path_to": {
      "type": "string",
      "description": "Reachability: return one shortest root-to-target chain (package dir or file.go#Symbol)."
    },
    "layers": {
      "type": "boolean",
      "description": "Reachability: return an SCC condensation (import or recursion cycles) instead of the summary."
    }
  },
  "required": [],
  "additionalProperties": false
}`)

// TopologyImpact performs a bidirectional BFS to assess blast radius around a symbol.
//
// Concurrency: Execute is safe for concurrent use.
type TopologyImpact struct {
	storeFn func() *topology.Store
	// callersFn, when set, supplies cross-file caller sites for a callable
	// centre symbol via the language server — the topology call graph is
	// intra-file only, so this fills the cross-file/cross-package caller gap.
	callersFn CrossFileCallersFunc
}

// NewTopologyImpact returns a new TopologyImpact tool.
func NewTopologyImpact(storeFn func() *topology.Store) *TopologyImpact {
	return &TopologyImpact{storeFn: storeFn}
}

// WithCrossFileCallers wires an LSP-backed cross-file caller resolver so the
// inward section is augmented with callers in other files. Returns the receiver
// for chaining; a nil fn leaves the tool topology-only.
func (t *TopologyImpact) WithCrossFileCallers(fn CrossFileCallersFunc) *TopologyImpact {
	t.callersFn = fn
	return t
}

func (*TopologyImpact) Name() string                 { return "topology_impact" }
func (*TopologyImpact) InputSchema() json.RawMessage { return topologyImpactSchema }
func (*TopologyImpact) Description() string {
	return "Blast radius around a named symbol: 'depends on' (outward) and 'depended on by' (inward), for assessing a refactor. Approximate (source=topology); for a function the inward side adds LSP-resolved cross-file callers (source=lsp) when available. mode=\"reachability\" answers entry-point reachability instead (Go only). Package granularity (default) follows production imports from main and topology_routes roots. granularity=\"function\" follows the admitted call graph from exact roots over production callers and durable derived cross-file edges to the full reachable closure: a lower bound that excludes test callers and unresolved dynamic calls. Both support path_to (one shortest chain) and layers (SCC condensation). Unsupported workspaces are refused, not reported unreachable. Outputs state their scope and limits and are byte-capped."
}

type topologyImpactArgs struct {
	Name        string   `json:"name"`
	Depth       int      `json:"depth"`
	MaxNodes    int      `json:"max_nodes"`
	MaxBytes    int      `json:"max_bytes"`
	EdgeKinds   []string `json:"edge_kinds"`
	Path        string   `json:"path"`
	Kind        string   `json:"kind"`
	Mode        string   `json:"mode"`
	Granularity string   `json:"granularity"`
	Roots       []string `json:"roots"`
	PathTo      string   `json:"path_to"`
	Layers      bool     `json:"layers"`
}

// modeReachability selects package-level reachability from entry points
// instead of the default single-symbol blast-radius analysis. See
// topology_reachability.go.
const modeReachability = "reachability"

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
	if a.Mode == modeReachability {
		if a.Granularity == "function" {
			return t.executeFunctionReachability(ctx, store, a)
		}
		return t.executeReachability(ctx, store, a)
	}
	result, alts, runErr := t.run(ctx, store, a)
	if runErr != nil {
		return "", runErr
	}
	callers := t.crossFileCallers(ctx, result)
	return formatImpactResult(result, a, alts, callers), nil
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
	if a.Granularity == "" {
		a.Granularity = "package"
	}
	return a, nil
}

func (a *topologyImpactArgs) validate() error {
	if a.Mode != "" && a.Mode != modeReachability {
		return fmt.Errorf("topology_impact: unknown mode %q (expected \"reachability\", or omit for the default blast-radius mode)", a.Mode)
	}
	if a.Mode == modeReachability {
		if a.Granularity != "package" && a.Granularity != "function" {
			return fmt.Errorf("topology_impact: unknown reachability granularity %q (expected \"package\" or \"function\")", a.Granularity)
		}
		return nil // name is not used in reachability mode; roots/path_to/layers stand alone
	}
	if a.Granularity != "" && a.Granularity != "package" {
		return errors.New("topology_impact: granularity requires mode=\"reachability\"")
	}
	// reachability-only fields silently doing nothing outside reachability mode
	// is exactly the failure this guards against: a caller who sets roots/
	// path_to/layers without mode="reachability" almost certainly meant to be
	// in reachability mode, and the classic path ignores all three.
	if len(a.Roots) > 0 || a.PathTo != "" || a.Layers {
		return errors.New(`topology_impact: roots/path_to/layers require mode="reachability" — they are ignored otherwise`)
	}
	if a.Name == "" {
		return errors.New("topology_impact: name is required")
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
		MaxNodes:  topology.ClampToolNodes(a.MaxNodes),
		MaxBytes:  topology.ClampToolBytes(a.MaxBytes),
		EdgeKinds: a.EdgeKinds,
	}
	result, err := store.ImpactFrom(ctx, cands[0], opts)
	if err != nil {
		return nil, nil, err
	}
	return result, cands[1:], nil
}

// crossFileCallers resolves cross-file caller sites for the centre symbol when a
// resolver is wired and the symbol is callable (function/method/test). Returns
// nil otherwise — the topology call graph already covers same-file callers.
func (t *TopologyImpact) crossFileCallers(ctx context.Context, result *topology.ImpactResult) []CallerSite {
	if t.callersFn == nil || result == nil {
		return nil
	}
	switch string(result.Centre.Kind) {
	case "function", "method", "test":
		return t.callersFn(ctx, result.Centre.Path, result.Centre.Name, result.Centre.StartLine, result.Centre.EndLine)
	default:
		return nil
	}
}

// maxCrossFileCallerSites caps the cross-file caller block so a heavily-called
// symbol cannot flood the output; the rest are summarised as a remainder.
const maxCrossFileCallerSites = 25

func formatImpactResult(result *topology.ImpactResult, a topologyImpactArgs, alts []topology.Node, callers []CallerSite) string {
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
	writeCrossFileCallers(&sb, callers)

	return strings.TrimRight(sb.String(), "\n") + topologyAmbiguityNote(a.Name, alts)
}

// writeCrossFileCallers appends the LSP-resolved cross-file caller block under
// the inward section. The topology call graph is intra-file, so these callers
// (in other files/packages) are not in result.DependedOnBy; they are labelled
// source=lsp to keep the provenance honest. A no-op when there are none.
func writeCrossFileCallers(sb *strings.Builder, callers []CallerSite) {
	if len(callers) == 0 {
		return
	}
	shown := callers
	if len(shown) > maxCrossFileCallerSites {
		shown = shown[:maxCrossFileCallerSites]
	}
	fmt.Fprintf(sb, "  cross-file callers (source=lsp, %d site(s)):\n", len(callers))
	for _, c := range shown {
		fmt.Fprintf(sb, "    %s:%d\n", c.Path, c.Line)
	}
	if len(callers) > len(shown) {
		fmt.Fprintf(sb, "    [+%d more]\n", len(callers)-len(shown))
	}
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
