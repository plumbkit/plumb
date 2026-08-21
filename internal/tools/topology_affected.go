package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/topology"
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
      "description": "Maximum PACKAGES to return. Default 50, which is well above a normal answer — raise it only for a change that fans out very widely. Tests are counted per package rather than listed individually, so this no longer caps test rows; the changed package always sorts first, so a cap cannot drop the package the edit landed in.",
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
	ws      WorkspaceFn // optional; enables the known-context memories join
}

// NewTopologyAffected returns a new TopologyAffected tool.
func NewTopologyAffected(storeFn func() *topology.Store) *TopologyAffected {
	return &TopologyAffected{storeFn: storeFn}
}

// WithMemories wires the workspace accessor so the response can append
// memories attached to the changed or affected files (test strategy, known
// risky areas).
func (t *TopologyAffected) WithMemories(ws WorkspaceFn) *TopologyAffected {
	t.ws = ws
	return t
}

func (*TopologyAffected) Name() string                 { return "topology_affected" }
func (*TopologyAffected) InputSchema() json.RawMessage { return topologyAffectedSchema }
func (*TopologyAffected) Description() string {
	return "After you change code, ask this which tests to run instead of running the whole " +
		"suite. Given changed files or symbols, it answers with PACKAGES to run — a ready " +
		"`go test ./pkg/...` line each, with the test count and why the package is " +
		"implicated — plus the individual test names in the package the change landed in. " +
		"A package is reached either by containing the change, or by importing a package " +
		"that does (cross-package import edges). Within a reached package every test is " +
		"counted, because co-location cannot tell which of them exercise the change: that " +
		"is the recall bias, and it is deliberate — a missed test is worse than an extra. " +
		"Results are heuristic; verify before relying. max_results bounds the number of " +
		"PACKAGES, and the changed package is always listed first so a cap cannot drop it. " +
		"Returns a clear message when topology is disabled."
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
	Reason     string // "dependency edge", "changed package", "imports the changed package"
}

// Reasons a directory is worth scanning for tests. They are ordered by how
// directly the change reaches them, which is also the order a caller should run
// them in.
const (
	reasonChanged  = "changed package"
	reasonImporter = "imports the changed package"
	reasonGraph    = "dependency edge"
)

// maxNamedTests caps the individually-named tests in the changed package. Past
// a few dozen the list stops informing a decision and starts costing context.
const maxNamedTests = 40

// affectedPackage is the unit a caller acts on: `go test` takes a package path,
// not a test name.
type affectedPackage struct {
	Dir    string
	Count  int
	Reason string
	Tests  []affectedTest
}

// aggregateTestsByPackage groups tests by directory, changed packages first,
// then by descending test count so the biggest run is visible.
func aggregateTestsByPackage(tests []affectedTest) []affectedPackage {
	byDir := map[string]*affectedPackage{}
	order := []string{}
	for _, ts := range tests {
		d := filepath.Dir(ts.Node.Path)
		p, ok := byDir[d]
		if !ok {
			p = &affectedPackage{Dir: d, Reason: ts.Reason}
			byDir[d] = p
			order = append(order, d)
		}
		// A package that contains the change outranks one that merely imports it,
		// whichever order the tests happened to arrive in.
		if ts.Reason == reasonChanged {
			p.Reason = reasonChanged
		}
		p.Count++
		p.Tests = append(p.Tests, ts)
	}
	out := make([]affectedPackage, 0, len(order))
	for _, d := range order {
		out = append(out, *byDir[d])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Reason == reasonChanged) != (out[j].Reason == reasonChanged) {
			return out[i].Reason == reasonChanged
		}
		return out[i].Count > out[j].Count
	})
	return out
}

// goTestTarget renders a directory as a runnable package pattern. A test at the
// workspace root has directory ".", which would otherwise print "./././...".
func goTestTarget(dir string) string {
	if dir == "." || dir == "" {
		return "go test ./..."
	}
	return "go test ./" + dir + "/..."
}

// firstChangedPackage returns the package the edit actually landed in, if any.
func firstChangedPackage(pkgs []affectedPackage) *affectedPackage {
	for i := range pkgs {
		if pkgs[i].Reason == reasonChanged {
			return &pkgs[i]
		}
	}
	return nil
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
	if store == nil {
		return topologyDisabledMessage(), nil
	}
	result, runErr := t.run(ctx, store, a)
	if runErr != nil {
		return "", runErr
	}
	out := formatAffectedResult(result, a)
	if t.ws != nil {
		out += relatedMemoriesSection(t.ws(ctx), affectedRefs(a, result))
	}
	return out, nil
}

// affectedRefs builds the CodeRef set for the memories join: the changed
// files and symbols the caller named, plus the affected nodes the traversal
// found.
func affectedRefs(a topologyAffectedArgs, result *affectedResult) []memory.CodeRef {
	refs := make([]memory.CodeRef, 0, len(a.Files)+len(a.Symbols)+len(result.Dependents)+len(result.Tests))
	for _, f := range a.Files {
		refs = append(refs, memory.CodeRef{File: filepath.ToSlash(f)})
	}
	for _, s := range a.Symbols {
		refs = append(refs, memory.CodeRef{SymbolName: s})
	}
	nodes := append(append([]topology.Node{}, result.Dependents...), testNodes(result.Tests)...)
	refs = append(refs, nodesToRefs(nodes)...)
	return refs
}

func testNodes(tests []affectedTest) []topology.Node {
	out := make([]topology.Node, 0, len(tests))
	for _, t := range tests {
		out = append(out, t.Node)
	}
	return out
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
		return errors.New("topology_affected: at least one file or symbol is required")
	}
	return nil
}

func (t *TopologyAffected) run(ctx context.Context, store *topology.Store, a topologyAffectedArgs) (*affectedResult, error) {
	if store == nil {
		return nil, nil
	}
	roots, seedDirs, err := resolveAffectedRoots(ctx, store, a)
	if err != nil {
		return nil, err
	}
	return collectAffected(ctx, store, roots, seedDirs, a.MaxResults)
}

// resolveAffectedRoots looks up the inward-BFS starting points: named symbols
// (via search) and every symbol of each changed file (via SymbolsInFile, a
// deterministic path lookup). It also returns each changed file's directory as a
// co-location seed, so a file with no indexed symbols still surfaces its sibling
// tests — the headline "I changed these files, which tests?" case.
func resolveAffectedRoots(ctx context.Context, store *topology.Store, a topologyAffectedArgs) (roots []topology.Node, seedDirs []string, err error) {
	for _, sym := range a.Symbols {
		results, serr := store.Search(ctx, sym, topology.SearchOpts{Limit: 5})
		if serr != nil {
			return nil, nil, fmt.Errorf("topology_affected: search %q: %w", sym, serr)
		}
		for _, r := range results {
			if r.Node.Name == sym {
				roots = append(roots, r.Node)
				break
			}
		}
	}
	for _, f := range a.Files {
		// The changed file's directory is always a co-location seed, even when the
		// file has no indexed symbols.
		seedDirs = append(seedDirs, filepath.Dir(f))
		nodes, ferr := store.SymbolsInFile(ctx, f)
		if ferr != nil {
			return nil, nil, fmt.Errorf("topology_affected: symbols in %q: %w", f, ferr)
		}
		if decls := declarationRoots(nodes); len(decls) > 0 {
			roots = append(roots, decls...)
			continue
		}
		// Fallback: not found by exact path; try an FTS5 search and suffix-match.
		results, serr := store.Search(ctx, f, topology.SearchOpts{Limit: 5})
		if serr != nil {
			return nil, nil, fmt.Errorf("topology_affected: search file %q: %w", f, serr)
		}
		for _, r := range results {
			if r.Node.Path == f || strings.HasSuffix(r.Node.Path, f) {
				roots = append(roots, r.Node)
				break
			}
		}
	}
	return roots, seedDirs, nil
}

// declarationRoots keeps only the nodes in a file that can actually change in a
// way another file depends on. SymbolsInFile returns every indexed node,
// including the file's `package` clause and one node per `import` — and an
// import node is named for the package it pulls in ("strings", "strconv"), which
// is both the most collision-prone name in any index and not a thing the edit
// changed. Seeding traversal from those is what made an unrelated package look
// affected.
func declarationRoots(nodes []topology.Node) []topology.Node {
	out := make([]topology.Node, 0, len(nodes))
	for _, n := range nodes {
		switch n.Kind {
		case topology.KindImport, topology.KindPackage, topology.KindFile:
			continue
		}
		out = append(out, n)
	}
	return out
}

func collectAffected(ctx context.Context, store *topology.Store, roots []topology.Node, seedDirs []string, maxResults int) (*affectedResult, error) {
	g := &affectedGather{store: store, maxResults: maxResults, seen: map[int64]bool{}, dirs: map[string]string{}}
	for _, d := range seedDirs {
		g.dirs[d] = reasonChanged
	}
	for _, root := range roots {
		g.dirs[filepath.Dir(root.Path)] = reasonChanged
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
	dirs       map[string]string // directory -> why it is implicated
	dependents []topology.Node
	tests      []affectedTest
	truncated  bool
}

func (g *affectedGather) total() int { return len(g.dependents) + len(g.tests) }

// fromGraph adds inward (dependedOnBy) neighbours of root: tests are flagged
// with their incident-edge confidence, other nodes become affected files and
// seed more directories for the co-location pass.
func (g *affectedGather) fromGraph(ctx context.Context, root topology.Node) {
	// ImpactFrom, not Impact: root is ALREADY resolved, and Impact would throw it
	// away and re-resolve the bare name against the whole index. That lookup has no
	// tie-break, so a common name lands on an arbitrary row in an unrelated package
	// and drags its entire test suite in. This is not hypothetical: the index here
	// holds 636 nodes named "strings" and 57 named "stats", so a change to
	// internal/stats/savings.go reported cmd/clientsmoke and internal/cli as
	// affected — 984 false positives — while pushing the one test that covers the
	// changed function out of the default result window entirely.
	nb, err := g.store.ImpactFrom(ctx, root, topology.ImpactOpts{
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
			g.tests = append(g.tests, affectedTest{Node: n, Confidence: c, Reason: reasonGraph})
		} else {
			g.dependents = append(g.dependents, n)
			// Do not demote a directory that is already the changed one: a package
			// that both contains the edit and is reached by an edge is still where
			// the change actually happened.
			if d := filepath.Dir(n.Path); g.dirs[d] == "" {
				g.dirs[d] = reasonImporter
			}
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
	// Deliberately NOT gated on g.truncated. It used to return early if the graph
	// pass had filled the budget, which meant a change with many graph dependents
	// produced zero co-located tests — and co-location is the only arm that can
	// name a test at all, since a test never lives in the file it exercises. That
	// turned a budget cap into a recall cliff.
	//
	// max_results now bounds PACKAGES, not test rows. Truncating a test list drops
	// coverage silently; truncating a package list drops a whole `go test` target
	// that the caller can see is missing.
	dirs := make([]string, 0, len(g.dirs))
	for d := range g.dirs {
		dirs = append(dirs, d)
	}
	// Changed packages first, so a cap can never drop the package the edit landed
	// in — the failure that made this bug harmful.
	sort.SliceStable(dirs, func(i, j int) bool {
		ci, cj := g.dirs[dirs[i]] == reasonChanged, g.dirs[dirs[j]] == reasonChanged
		if ci != cj {
			return ci
		}
		return dirs[i] < dirs[j]
	})
	if g.maxResults > 0 && len(dirs) > g.maxResults {
		dirs = dirs[:g.maxResults]
		g.truncated = true
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
		reason := g.dirs[filepath.Dir(n.Path)]
		if reason == "" {
			reason = reasonImporter
		}
		g.tests = append(g.tests, affectedTest{Node: n, Confidence: colocatedConfidence, Reason: reason})
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
		return "topology_affected: none of the given files or symbols are in the index"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "topology affected: %d files, %d symbols changed\n",
		len(a.Files), len(a.Symbols))
	sb.WriteString("source=topology — heuristic, biased toward recall: a missed test is worse " +
		"than an extra. A package is reached by containing the change, or by importing a " +
		"package that does; within a reached package every test is listed, because " +
		"co-location cannot say which ones exercise the change. Verify before relying.\n\n")

	if len(result.Tests) == 0 {
		sb.WriteString("likely affected tests: (none found)\n")
		if len(result.Dependents) > 0 {
			fmt.Fprintf(&sb, "\naffected files (%d):\n", len(result.Dependents))
			for _, n := range result.Dependents {
				fmt.Fprintf(&sb, "  %s %s — %s\n", string(n.Kind), n.Name, n.Path)
			}
		}
		return strings.TrimRight(sb.String(), "\n")
	}

	// Aggregate by package. Enumerating every test is what made this response
	// 298 KB for a one-line change: 2,546 lines carrying the same two labels. The
	// unit a caller acts on is the package path, because that is what `go test`
	// takes, so lead with a runnable command per package.
	pkgs := aggregateTestsByPackage(result.Tests)
	fmt.Fprintf(&sb, "run these packages (%d):\n", len(pkgs))
	for _, p := range pkgs {
		fmt.Fprintf(&sb, "  %-42s %5d tests   %s\n", goTestTarget(p.Dir), p.Count, p.Reason)
	}

	// Name individual tests only where naming them helps: the changed package.
	// Elsewhere every test carries an identical label, so the list is noise.
	if named := firstChangedPackage(pkgs); named != nil {
		fmt.Fprintf(&sb, "\ntests in the changed package (%d):\n", named.Count)
		shown := named.Tests
		if len(shown) > maxNamedTests {
			shown = shown[:maxNamedTests]
		}
		for _, ts := range shown {
			fmt.Fprintf(&sb, "  %s — %s L%d\n", ts.Node.Name, ts.Node.Path, ts.Node.StartLine)
		}
		if rest := named.Count - len(shown); rest > 0 {
			fmt.Fprintf(&sb, "  … (+%d more in this package)\n", rest)
		}
	}

	if result.Truncated {
		sb.WriteString("\n[truncated: max_results reached — raise max_results for the full package list]\n")
	}
	return withTruncationBanner(strings.TrimRight(sb.String(), "\n"), cutPackagesNotice(result, a))
}

// cutPackagesNotice describes the cut for the leading banner, or "" when the
// answer is complete. Named for what it reports rather than for the word
// "truncate": it shortens no string, and the arch guard that watches for
// re-implemented string truncation is right to read that name as a claim.
func cutPackagesNotice(result *affectedResult, a topologyAffectedArgs) string {
	if !result.Truncated {
		return ""
	}
	return fmt.Sprintf("packages were cut at max_results=%d. Some packages that should be "+
		"tested are NOT listed below. Raise max_results for the full set.", a.MaxResults)
}
