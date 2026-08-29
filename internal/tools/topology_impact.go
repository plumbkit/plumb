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
      "description": "Symbol name or qualified name to analyse. Must exist in the topology index. Required unless mode=\"reachability\"."
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
    },
    "mode": {
      "type": "string",
      "description": "Optional. \"reachability\" switches from the default single-symbol blast-radius analysis to entry-point reachability. Go-only for now; roots/path_to/layers require this mode."
    },
    "granularity": {
      "type": "string",
      "enum": ["package", "function"],
      "default": "package",
      "description": "Requires mode=\"reachability\". Default package follows production import edges. function follows the admitted Go call graph outward from exact callable roots; test-file callers are excluded and unresolved/dynamic calls are disclosed."
    },
    "roots": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Requires mode=\"reachability\". package granularity accepts package directories or \"main\". function granularity accepts exact file.go#Symbol selectors or \"main\"; omit for defaults (package main roots plus candidate-seeded topology_routes roots)."
    },
    "path_to": {
      "type": "string",
      "description": "Requires mode=\"reachability\". When set, the response is the single shortest root -> target chain; use a package directory for package granularity or file.go#Symbol for function granularity."
    },
    "layers": {
      "type": "boolean",
      "description": "Requires mode=\"reachability\". When true, the response is an SCC condensation of the reachable subgraph — package import cycles or function recursion depending on granularity — instead of the summary."
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
	return "Bidirectional BFS blast-radius analysis around a named symbol. " +
		"Returns two sections: 'depends on' (outward — what the symbol depends on) and " +
		"'depended on by' (inward — what depends on this symbol). " +
		"Primary use: assess blast radius before a refactor. Source is 'topology' (approximate); " +
		"the topology call graph is intra-file, so for a function/method the inward section is " +
		"augmented with a 'cross-file callers' block resolved via the language server (source=lsp) " +
		"when one is available. " +
		"mode=\"reachability\" switches to entry-point reachability. The default package " +
		"granularity follows production import edges from package-main roots plus candidate-seeded " +
		"topology_routes roots; Go _test.go importers are excluded, and unsupported/polyglot " +
		"workspaces are refused rather than reported as falsely unreachable. " +
		"Set granularity=\"function\" for the additive Go-only admitted partial static call graph: " +
		"it uses exact callable roots, production callers, durable derived cross-file edges, and " +
		"the full reachable closure. Unresolved receiver/dynamic calls, test callers, unsupported " +
		"languages, and unindexed roots remain outside that lower-bound answer. " +
		"Each granularity supports the default summary, path_to (one shortest root-to-target chain), " +
		"and layers (SCC condensation; import cycles for package or recursion cycles for function). " +
		"All outputs disclose their scope and known limitations, and responses are byte-capped. " +
		"Returns a clear message when topology is disabled or the symbol is not in the index."
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
		return t.callersFn(ctx, result.Centre.Path, result.Centre.Name)
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
