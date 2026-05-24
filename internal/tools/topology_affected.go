package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/golimpio/plumb/internal/topology"
)

// colocatedConfidence labels tests found only by sitting in the same directory
// as a changed/affected file (no dependency edge connects them). Lower than the
// heuristic call/import edge baseline (0.8) but high enough to surface.
const colocatedConfidence = 0.5

// graphEdgeBaseline is the floor confidence for a test reached through the
// dependency graph whose connecting edge was filtered from the subgraph.
const graphEdgeBaseline = 0.8

var topologyAffectedSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "files": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Workspace-relative file paths to treat as change roots."
    },
    "symbols": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Symbol names to treat as change roots."
    },
    "max_results": {
      "type": "integer",
      "description": "Maximum affected nodes to return. Default 50.",
      "default": 50
    }
  },
  "additionalProperties": false
}`)

// TopologyAffected traverses inward edges from changed files/symbols to report
// dependents and likely affected tests.
//
// Concurrency: Execute is safe for concurrent use.
type TopologyAffected struct {
	storeFn func() *topology.Store
}

// NewTopologyAffected returns a new TopologyAffected tool.
func NewTopologyAffected(storeFn func() *topology.Store) *TopologyAffected {
	return &TopologyAffected{storeFn: storeFn}
}

func (*TopologyAffected) Name() string                 { return "topology_affected" }
func (*TopologyAffected) InputSchema() json.RawMessage { return topologyAffectedSchema }
func (*TopologyAffected) Description() string {
	return "THE headline topology tool: after you change code, ask this which tests to run " +
		"instead of running the whole suite. Given changed files or symbols, it returns the " +
		"likely affected files and tests by traversing inward dependency edges AND by " +
		"co-location (tests in the same directory as a changed/affected file — which catches " +
		"sibling test files the call graph alone misses). " +
		"Results are heuristic and biased toward recall (a missed test is worse than an extra); " +
		"every test carries a confidence and the reason it was flagged. No language server gives " +
		"this answer. Returns a clear message when topology is disabled."
}

type topologyAffectedArgs struct {
	Files      []string `json:"files"`
	Symbols    []string `json:"symbols"`
	MaxResults int      `json:"max_results"`
}

// affectedTest is a test likely impacted by a change, with how it was reached
// and how sure we are.
type affectedTest struct {
	Node       topology.Node
	Confidence float64
	Reason     string // "dependency edge" or "co-located"
}

// affectedResult collects dependents and likely-affected tests.
type affectedResult struct {
	Dependents []topology.Node
	Tests      []affectedTest
	Truncated  bool
}

func (t *TopologyAffected) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseTopologyAffectedArgs(raw)
	if err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	store := t.storeFn()
	result, runErr := t.run(ctx, store, a)
	if runErr != nil {
		return "", runErr
	}
	return formatAffectedResult(result, a), nil
}

func parseTopologyAffectedArgs(raw json.RawMessage) (topologyAffectedArgs, error) {
	var a topologyAffectedArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("topology_affected: invalid arguments: %w", err)
	}
	if a.MaxResults <= 0 {
		a.MaxResults = 50
	}
	return a, nil
}

func (a *topologyAffectedArgs) validate() error {
	if len(a.Files) == 0 && len(a.Symbols) == 0 {
		return fmt.Errorf("topology_affected: at least one file or symbol is required")
	}
	return nil
}

func (t *TopologyAffected) run(ctx context.Context, store *topology.Store, a topologyAffectedArgs) (*affectedResult, error) {
	if store == nil {
		return nil, nil
	}
	roots, err := resolveAffectedRoots(ctx, store, a)
	if err != nil {
		return nil, err
	}
	return collectAffected(ctx, store, roots, a.MaxResults)
}

// resolveAffectedRoots looks up all named symbols and file-path nodes to use as
// inward-BFS starting points.
func resolveAffectedRoots(ctx context.Context, store *topology.Store, a topologyAffectedArgs) ([]topology.Node, error) {
	var roots []topology.Node
	for _, sym := range a.Symbols {
		opts := topology.SearchOpts{Limit: 5}
		results, err := store.Search(ctx, sym, opts)
		if err != nil {
			return nil, fmt.Errorf("topology_affected: search %q: %w", sym, err)
		}
		for _, r := range results {
			if r.Node.Name == sym {
				roots = append(roots, r.Node)
				break
			}
		}
	}
	for _, f := range a.Files {
		opts := topology.SearchOpts{Limit: 5}
		results, err := store.Search(ctx, f, opts)
		if err != nil {
			return nil, fmt.Errorf("topology_affected: search file %q: %w", f, err)
		}
		for _, r := range results {
			if r.Node.Path == f || strings.HasSuffix(r.Node.Path, f) {
				roots = append(roots, r.Node)
				break
			}
		}
	}
	return roots, nil
}

func collectAffected(ctx context.Context, store *topology.Store, roots []topology.Node, maxResults int) (*affectedResult, error) {
	g := &affectedGather{store: store, maxResults: maxResults, seen: map[int64]bool{}, dirs: map[string]bool{}}
	for _, root := range roots {
		g.dirs[filepath.Dir(root.Path)] = true
		g.fromGraph(ctx, root)
		if g.truncated {
			break
		}
	}
	g.fromColocation(ctx)
	g.sortTests()
	return &affectedResult{Dependents: g.dependents, Tests: g.tests, Truncated: g.truncated}, nil
}

// affectedGather accumulates affected nodes across roots, de-duplicating by ID
// and tracking the directories worth scanning for co-located tests.
type affectedGather struct {
	store      *topology.Store
	maxResults int
	seen       map[int64]bool
	dirs       map[string]bool
	dependents []topology.Node
	tests      []affectedTest
	truncated  bool
}

func (g *affectedGather) total() int { return len(g.dependents) + len(g.tests) }

// fromGraph adds inward (dependedOnBy) neighbours of root: tests are flagged
// with their incident-edge confidence, other nodes become affected files and
// seed more directories for the co-location pass.
func (g *affectedGather) fromGraph(ctx context.Context, root topology.Node) {
	nb, err := g.store.Impact(ctx, root.Name, topology.ImpactOpts{
		Depth:     2,
		MaxNodes:  g.maxResults,
		MaxBytes:  100000,
		EdgeKinds: []string{"calls", "imports", "contains"},
	})
	if err != nil {
		return
	}
	conf := incidentConfidence(nb.DependedOnBy.Edges)
	for _, n := range nb.DependedOnBy.Nodes {
		if g.seen[n.ID] {
			continue
		}
		g.seen[n.ID] = true
		if n.Kind == topology.KindTest {
			c := conf[n.ID]
			if c == 0 {
				c = graphEdgeBaseline
			}
			g.tests = append(g.tests, affectedTest{Node: n, Confidence: c, Reason: "dependency edge"})
		} else {
			g.dependents = append(g.dependents, n)
			g.dirs[filepath.Dir(n.Path)] = true
		}
		if g.total() >= g.maxResults {
			g.truncated = true
			return
		}
	}
}

// fromColocation adds tests that sit in a changed/affected directory but were
// not reached through the graph (the recall booster).
func (g *affectedGather) fromColocation(ctx context.Context) {
	if g.truncated {
		return
	}
	dirs := make([]string, 0, len(g.dirs))
	for d := range g.dirs {
		dirs = append(dirs, d)
	}
	tests, err := g.store.TestsInDirs(ctx, dirs)
	if err != nil {
		return
	}
	for _, n := range tests {
		if g.seen[n.ID] {
			continue
		}
		g.seen[n.ID] = true
		g.tests = append(g.tests, affectedTest{Node: n, Confidence: colocatedConfidence, Reason: "co-located"})
		if g.total() >= g.maxResults {
			g.truncated = true
			return
		}
	}
}

// sortTests orders tests by descending confidence so graph-reached (higher)
// precede co-located (lower); insertion order breaks ties.
func (g *affectedGather) sortTests() {
	sort.SliceStable(g.tests, func(i, j int) bool {
		return g.tests[i].Confidence > g.tests[j].Confidence
	})
}

// incidentConfidence maps each node ID to the highest confidence of any edge
// incident to it within the affected subgraph — an approximation of how
// strongly that node is linked to the change.
func incidentConfidence(edges []topology.Edge) map[int64]float64 {
	m := map[int64]float64{}
	for _, e := range edges {
		if e.Confidence > m[e.FromID] {
			m[e.FromID] = e.Confidence
		}
		if e.Confidence > m[e.ToID] {
			m[e.ToID] = e.Confidence
		}
	}
	return m
}

func formatAffectedResult(result *affectedResult, a topologyAffectedArgs) string {
	if result == nil {
		return "topology indexing is disabled for this session\n" +
			"Set [topology] enabled = true in .plumb/config.toml to enable."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "topology affected: %d files, %d symbols changed\n",
		len(a.Files), len(a.Symbols))
	sb.WriteString("source=topology — heuristic, biased toward recall: a missed test is worse " +
		"than an extra, so co-located tests are included. Confidence: 1.0 certain (containment), " +
		"0.8 dependency edge, 0.5 co-located (same directory, no edge). Verify before relying.\n\n")

	if len(result.Dependents) == 0 {
		sb.WriteString("affected files: (none found)\n")
	} else {
		fmt.Fprintf(&sb, "affected files (%d):\n", len(result.Dependents))
		for _, n := range result.Dependents {
			fmt.Fprintf(&sb, "  %s %s — %s", string(n.Kind), n.Name, n.Path)
			if n.StartLine > 0 {
				fmt.Fprintf(&sb, " L%d", n.StartLine)
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n")
	if len(result.Tests) == 0 {
		sb.WriteString("likely affected tests: (none found)\n")
	} else {
		fmt.Fprintf(&sb, "likely affected tests (%d, highest confidence first):\n", len(result.Tests))
		for _, ts := range result.Tests {
			fmt.Fprintf(&sb, "  %s — %s L%d  [%s, confidence %.1f]\n",
				ts.Node.Name, ts.Node.Path, ts.Node.StartLine, ts.Reason, ts.Confidence)
		}
	}

	if result.Truncated {
		sb.WriteString("\n[truncated: max_results reached]\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
