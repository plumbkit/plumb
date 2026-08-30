package tools

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

func (t *TopologyImpact) executeFunctionReachability(ctx context.Context, store *topology.Store, a topologyImpactArgs) (string, error) {
	pkgs, err := store.NodesByKind(ctx, topology.KindPackage)
	if err != nil {
		return "", fmt.Errorf("topology_impact: function reachability: admission: %w", err)
	}
	admitted := false
	var scopeNote string
	for _, p := range pkgs {
		if p.Language != "go" {
			continue
		}
		decision, decisionErr := store.AdmitCallGraph(ctx, topology.CallGraphSubject{Language: p.Language, Path: p.Path})
		if decisionErr != nil {
			return "", fmt.Errorf("topology_impact: function reachability: admission: %w", decisionErr)
		}
		if decision.Admitted {
			admitted = true
			scopeNote = decision.ScopeNote
			break
		}
	}
	if !admitted {
		return "topology_impact: function reachability: cross-file call edges are unavailable for this workspace; the admitted topology call graph is Go-only and requires an indexed Go package node", nil
	}
	g, err := store.FunctionGraph(ctx, "go", true)
	if err != nil {
		return "", fmt.Errorf("topology_impact: function reachability: %w", err)
	}
	roots, candidates, rootNote, err := t.resolveFunctionRoots(ctx, store, g, a.Roots)
	if err != nil {
		return "", err
	}
	if len(roots) == 0 {
		return "topology_impact: function reachability: no callable roots resolved" + rootNote, nil
	}
	res := topology.ReachableFunctions(g, roots)
	status := store.Status().CallGraph
	switch {
	case a.PathTo != "":
		return formatFunctionPath(g, res, a.PathTo, candidates, rootNote, status, scopeNote), nil
	case a.Layers:
		return formatFunctionLayers(g, res, candidates, rootNote, status, scopeNote), nil
	default:
		return formatFunctionSummary(g, res, candidates, rootNote, status, scopeNote), nil
	}
}

func (t *TopologyImpact) resolveFunctionRoots(ctx context.Context, store *topology.Store, g *topology.FunctionGraph, selectors []string) ([]int64, map[int64]bool, string, error) {
	var roots []int64
	var unresolved []string
	candidates := map[int64]bool{}
	if len(selectors) == 0 {
		var err error
		roots, candidates, err = t.defaultFunctionRoots(ctx, store, g)
		if err != nil {
			return nil, nil, "", err
		}
	} else {
		var err error
		roots, unresolved, err = explicitFunctionRoots(g, selectors)
		if err != nil {
			return nil, nil, "", err
		}
	}
	roots = dedupFunctionIDs(g, roots)
	sort.Slice(roots, func(i, j int) bool { return functionNodeLess(g.Nodes[roots[i]], g.Nodes[roots[j]]) })
	note := ""
	if len(unresolved) > 0 {
		note = fmt.Sprintf(" (unresolved roots, skipped: %s)", strings.Join(unresolved, ", "))
	}
	if len(selectors) == 0 && len(roots) == 0 {
		note = " (no package-main main function or route candidate found — pass roots as file.go#Symbol)"
	}
	return roots, candidates, note, nil
}

func functionMainRoots(g *topology.FunctionGraph) []int64 {
	var roots []int64
	for id, n := range g.Nodes {
		if n.Name == "main" && g.MainDirs[path.Dir(n.Path)] {
			roots = append(roots, id)
		}
	}
	return roots
}

func (t *TopologyImpact) defaultFunctionRoots(ctx context.Context, store *topology.Store, g *topology.FunctionGraph) ([]int64, map[int64]bool, error) {
	roots := functionMainRoots(g)
	candidates := map[int64]bool{}
	routes, err := (&TopologyRoutes{}).run(ctx, store, topologyRoutesArgs{Limit: reachabilityRouteCandidateLimit})
	if err != nil {
		return nil, nil, fmt.Errorf("topology_impact: function reachability: route candidates: %w", err)
	}
	for _, route := range routes {
		if _, ok := g.Nodes[route.Node.ID]; ok {
			roots = append(roots, route.Node.ID)
			candidates[route.Node.ID] = true
		}
	}
	return roots, candidates, nil
}

func explicitFunctionRoots(g *topology.FunctionGraph, selectors []string) ([]int64, []string, error) {
	var roots []int64
	var unresolved []string
	for _, selector := range selectors {
		if selector == "main" {
			roots = append(roots, functionMainRoots(g)...)
			continue
		}
		matches := resolveFunctionSelector(g, selector)
		if len(matches) == 0 {
			unresolved = append(unresolved, selector)
			continue
		}
		if len(matches) > 1 {
			return nil, nil, fmt.Errorf("topology_impact: function reachability: root %q is ambiguous (%s)", selector, joinFunctionSelectors(matches))
		}
		roots = append(roots, matches[0].ID)
	}
	return roots, unresolved, nil
}

func resolveFunctionSelector(g *topology.FunctionGraph, selector string) []topology.Node {
	var out []topology.Node
	if i := strings.LastIndex(selector, "#"); i >= 0 {
		wantPath, wantName := path.Clean(selector[:i]), selector[i+1:]
		for _, n := range g.Nodes {
			if path.Clean(n.Path) == wantPath && n.Name == wantName {
				out = append(out, n)
			}
		}
	} else {
		for _, n := range g.Nodes {
			if n.Name == selector || n.Qualified == selector {
				out = append(out, n)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return functionNodeLess(out[i], out[j]) })
	return out
}

func dedupFunctionIDs(g *topology.FunctionGraph, ids []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		if _, ok := g.Nodes[id]; !ok {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func functionNodeLess(a, b topology.Node) bool {
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.StartLine != b.StartLine {
		return a.StartLine < b.StartLine
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.ID < b.ID
}

func joinFunctionSelectors(nodes []topology.Node) string {
	parts := make([]string, len(nodes))
	for i, n := range nodes {
		parts[i] = fmt.Sprintf("%s L%d", path.Clean(n.Path)+"#"+n.Name, n.StartLine)
	}
	return strings.Join(parts, ", ")
}

func formatFunctionHeader(sb *strings.Builder, res *topology.FunctionReachabilityResult, g *topology.FunctionGraph, candidates map[int64]bool, rootNote string, status topology.CallGraphStatus, scopeNote string) {
	rootParts := make([]string, len(res.Roots))
	for i, id := range res.Roots {
		n := g.Nodes[id]
		rootParts[i] = path.Clean(n.Path) + "#" + n.Name
		if candidates[id] {
			rootParts[i] += " (candidate-seeded)"
		}
	}
	fmt.Fprintf(sb, "topology reachability: function-level (static call edges, Go only; production callers; lower bound)\n")
	if scopeNote != "" {
		fmt.Fprintf(sb, "call graph scope: %s\n", scopeNote)
	}
	fmt.Fprintf(sb, "roots (%d): %s%s\n", len(rootParts), strings.Join(rootParts, ", "), rootNote)
	if status.CallSites == 0 {
		sb.WriteString("call graph census: no recorded call sites are published yet; unresolved and dynamic calls are not followed\n\n")
		return
	}
	fmt.Fprintf(sb, "call graph census: %d resolved cross-file edge(s), %d recorded site(s), %d from non-test callers; unresolved receiver=%d external=%d unmatched=%d no-caller=%d; test callers excluded\n\n",
		status.Resolved, status.CallSites, status.ResolvedNonTest, status.UnresolvedReceiver,
		status.ExternalPackage, status.UnmatchedTarget, status.NoCallerNode)
}

func formatFunctionNode(n topology.Node) string {
	return fmt.Sprintf("%s#%s L%d", path.Clean(n.Path), n.Name, n.StartLine)
}

func formatFunctionSummary(g *topology.FunctionGraph, res *topology.FunctionReachabilityResult, candidates map[int64]bool, rootNote string, status topology.CallGraphStatus, scopeNote string) string {
	var sb strings.Builder
	formatFunctionHeader(&sb, res, g, candidates, rootNote, status, scopeNote)
	reached := make([]topology.Node, 0, len(res.Reachable))
	for id := range res.Reachable {
		reached = append(reached, g.Nodes[id])
	}
	sort.Slice(reached, func(i, j int) bool { return functionNodeLess(reached[i], reached[j]) })
	fmt.Fprintf(&sb, "reachable: %d callable(s)\n", len(reached))
	for i, n := range reached {
		if i >= reachabilitySampleLimit {
			break
		}
		fmt.Fprintf(&sb, "  %s\n", formatFunctionNode(n))
	}
	if len(reached) > reachabilitySampleLimit {
		fmt.Fprintf(&sb, "  [+%d more]\n", len(reached)-reachabilitySampleLimit)
	}
	unreached := make([]topology.Node, 0, len(g.Nodes)-len(reached))
	for id, n := range g.Nodes {
		if !res.Reachable[id] {
			unreached = append(unreached, n)
		}
	}
	sort.Slice(unreached, func(i, j int) bool { return functionNodeLess(unreached[i], unreached[j]) })
	fmt.Fprintf(&sb, "\nnot reached: %d callable(s) (not proof of dead code; unresolved receiver/dynamic calls are absent)\n", len(unreached))
	for i, n := range unreached {
		if i >= reachabilitySampleLimit {
			break
		}
		fmt.Fprintf(&sb, "  %s\n", formatFunctionNode(n))
	}
	if len(unreached) > reachabilitySampleLimit {
		fmt.Fprintf(&sb, "  [+%d more]\n", len(unreached)-reachabilitySampleLimit)
	}
	return capReachabilityBytes(sb.String())
}

func formatFunctionPath(g *topology.FunctionGraph, res *topology.FunctionReachabilityResult, selector string, candidates map[int64]bool, rootNote string, status topology.CallGraphStatus, scopeNote string) string {
	var sb strings.Builder
	formatFunctionHeader(&sb, res, g, candidates, rootNote, status, scopeNote)
	matches := resolveFunctionSelector(g, selector)
	if len(matches) == 0 {
		fmt.Fprintf(&sb, "path_to %q: not an indexed production callable\n", selector)
		return capReachabilityBytes(sb.String())
	}
	if len(matches) > 1 {
		fmt.Fprintf(&sb, "path_to %q: ambiguous (%s)\n", selector, joinFunctionSelectors(matches))
		return capReachabilityBytes(sb.String())
	}
	chain, found := topology.FunctionPathTo(g, res, matches[0].ID)
	if !found {
		fmt.Fprintf(&sb, "path to %s: no path — not reached from the given roots\n", formatFunctionNode(matches[0]))
		return capReachabilityBytes(sb.String())
	}
	parts := make([]string, len(chain))
	for i, n := range chain {
		parts[i] = formatFunctionNode(n)
	}
	fmt.Fprintf(&sb, "path to %s:\n  %s\n", formatFunctionNode(matches[0]), strings.Join(parts, " -> "))
	return capReachabilityBytes(sb.String())
}

func formatFunctionLayers(g *topology.FunctionGraph, res *topology.FunctionReachabilityResult, candidates map[int64]bool, rootNote string, status topology.CallGraphStatus, scopeNote string) string {
	var sb strings.Builder
	formatFunctionHeader(&sb, res, g, candidates, rootNote, status, scopeNote)
	sccs := topology.CondenseFunctionSCCs(g, res.Reachable)
	cycles := 0
	for _, s := range sccs {
		if s.Cycle {
			cycles++
		}
	}
	fmt.Fprintf(&sb, "layers: %d function SCC(s) over %d reachable callable(s), %d flagged as recursive cycles\n", len(sccs), len(res.Reachable), cycles)
	shown := 0
	current := -1
	for _, s := range sccs {
		if shown >= reachabilityLayerLimit {
			fmt.Fprintf(&sb, "  [+%d more SCC(s)]\n", len(sccs)-shown)
			break
		}
		if s.Layer != current {
			current = s.Layer
			fmt.Fprintf(&sb, "  layer %d:\n", current)
		}
		label := ""
		if s.Cycle {
			label = "  [cycle]"
		}
		names := make([]string, len(s.Nodes))
		for i, n := range s.Nodes {
			names[i] = formatFunctionNode(n)
		}
		fmt.Fprintf(&sb, "    %s%s\n", strings.Join(names, ", "), label)
		shown++
	}
	return capReachabilityBytes(sb.String())
}
