package tools

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

// reachabilityConfidenceLine opens every reachability response. The import
// graph is real and cross-file (linkImports), but there is no cross-package
// call graph yet — this label keeps that honest rather than implying a
// function-level answer this mode cannot give. "production imports only"
// discloses the other direction a caller must know: an edge whose importer is
// a Go _test.go file is excluded (see PackageGraph's doc in
// internal/topology/reachability.go) — a package pulled in only by a test
// helper/fixture is reported unreachable here, and every SCC>1 the layers
// shape can report is a real production cycle, never a test-only artefact.
const reachabilityConfidenceLine = "package-level (import edges, production imports only — Go _test.go importers excluded); function-level unavailable — see roadmap"

// reachabilityGoOnlyMessage is returned when package directories were indexed,
// zero directory-level edges could be folded at all, AND there is no
// independent evidence (PackageGraph.HasGoSignal) that this is a Go
// workspace. TotalEdges()==0 alone is not enough to conclude "wrong
// language": a genuinely small Go workspace can have zero FOLDABLE edges too
// (every cross-package import is stdlib-only, or its only cross-package
// import lives in a _test.go file, which isTestGoImporter deliberately
// excludes) — gating on edge count alone told real Go workspaces shaped like
// that they "weren't Go". loadPackageEdges needs a KindPackage node to carry
// its own outward "imports" edge to a KindImport node in the same file —
// today only the Go extractor emits that shape (extractors/golang/
// extractor.go). A C#/PHP/Scala/Elixir/etc workspace still indexes
// KindPackage nodes (so g.Dirs is non-empty) but produces none of those
// edges AND carries no Go signal, which would otherwise make every single
// package in the codebase look "unreachable" — a confident, wrong answer.
// Refusing is deliberately louder than that.
const reachabilityGoOnlyMessage = "topology_impact: reachability: %d package director(ies) indexed but zero import edges were foldable, and nothing in the index looks like Go — package-level reachability needs Go's per-file `package X` + `import` node/edge shape (extractors/golang), which other languages do not yet emit. Refusing rather than reporting every package unreachable; this mode is Go-only for now."

// reachabilityMaxBytes hard-caps every reachability response shape. Chosen
// well under the ~5 KB budget in PLAN-371 so the truncation note itself never
// pushes a borderline response over the line.
const reachabilityMaxBytes = 4800

// reachabilitySampleLimit caps how many packages the reachable/unreachable
// summary lists per bucket — counts first, samples capped, never the raw
// graph (the aggregateTestsByPackage pattern topology_affected.go uses).
const reachabilitySampleLimit = 10

// reachabilityLayerLimit caps how many SCCs the layers shape lists.
const reachabilityLayerLimit = 60

// reachabilityRouteCandidateLimit bounds how many topology_routes candidates
// seed the default root set — this is a root count, not a result list, so it
// stays small.
const reachabilityRouteCandidateLimit = 50

// executeReachability handles topology_impact mode="reachability": package
// (directory) level reachability from entry points, over "imports" edges
// only. It is a distinct traversal from the single-symbol blast-radius path
// above — see docs/topology.md's reachability section for why a depth-capped
// node BFS is the wrong tool here (it would silently under-report
// reachability on any dependency chain deeper than the neighbourhood cap).
func (t *TopologyImpact) executeReachability(ctx context.Context, store *topology.Store, a topologyImpactArgs) (string, error) {
	g, err := store.PackageGraph(ctx)
	if err != nil {
		return "", fmt.Errorf("topology_impact: reachability: %w", err)
	}
	if len(g.Dirs) == 0 {
		return "topology_impact: reachability: no indexed packages", nil
	}
	if len(g.Dirs) > 1 && g.TotalEdges() == 0 && !g.HasGoSignal {
		return fmt.Sprintf(reachabilityGoOnlyMessage, len(g.Dirs)), nil
	}

	roots, candidateDirs, rootNote, err := t.resolveReachabilityRoots(ctx, store, g, a)
	if err != nil {
		return "", err
	}
	if len(roots) == 0 {
		return "topology_impact: reachability: no root packages resolved" + rootNote, nil
	}

	res := topology.ReachableFrom(g, roots)

	switch {
	case a.PathTo != "":
		return formatReachabilityPath(g, res, a.PathTo, candidateDirs, rootNote), nil
	case a.Layers:
		return formatReachabilityLayers(g, res, candidateDirs, rootNote), nil
	default:
		return formatReachabilitySummary(g, res, candidateDirs, rootNote), nil
	}
}

// resolveReachabilityRoots turns a.Roots into resolved package directories.
// Per the card's root-resolution trap, every root is resolved through the
// PackageGraph's own directory index (built from the same node data
// linkImports validated) rather than by re-deriving identity from a raw
// string — an explicit root that does not match any indexed directory is
// reported unresolved rather than silently dropped or guessed at.
func (t *TopologyImpact) resolveReachabilityRoots(ctx context.Context, store *topology.Store, g *topology.PackageGraph, a topologyImpactArgs) (roots []string, candidateDirs map[string]bool, note string, err error) {
	candidateDirs = map[string]bool{}
	if len(a.Roots) == 0 {
		roots = append(roots, g.MainDirs()...)
		cands, cerr := reachabilityRouteCandidates(ctx, store)
		if cerr != nil {
			return nil, nil, "", cerr
		}
		for _, d := range cands {
			resolved, ok := g.ResolveDir(d)
			if !ok {
				continue
			}
			candidateDirs[resolved] = true
			roots = append(roots, resolved)
		}
		roots = dedupSortedStrings(roots)
		if len(roots) == 0 {
			note = " (no `package main` directory and no topology_routes candidate found — pass roots explicitly)"
		}
		return roots, candidateDirs, note, nil
	}

	seen := map[string]bool{}
	var unresolved []string
	for _, r := range a.Roots {
		if r == "main" {
			for _, d := range g.MainDirs() {
				if !seen[d] {
					seen[d] = true
					roots = append(roots, d)
				}
			}
			continue
		}
		resolved, ok := g.ResolveDir(r)
		if !ok {
			unresolved = append(unresolved, r)
			continue
		}
		if !seen[resolved] {
			seen[resolved] = true
			roots = append(roots, resolved)
		}
	}
	sort.Strings(roots)
	if len(unresolved) > 0 {
		note = fmt.Sprintf(" (unresolved roots, skipped: %s)", strings.Join(unresolved, ", "))
	}
	return roots, candidateDirs, note, nil
}

// reachabilityRouteCandidates reuses topology_routes' entry-point pattern
// matching to seed additional roots (HTTP handlers, CLI commands, …) beyond
// `package main` directories — labelled candidate-seeded by the caller since,
// per topology_routes' own contract, these are name/signature matches, not
// confirmed entry points.
func reachabilityRouteCandidates(ctx context.Context, store *topology.Store) ([]string, error) {
	routes, err := (&TopologyRoutes{}).run(ctx, store, topologyRoutesArgs{Limit: reachabilityRouteCandidateLimit})
	if err != nil {
		return nil, fmt.Errorf("topology_impact: reachability: route candidates: %w", err)
	}
	seen := map[string]bool{}
	var dirs []string
	for _, r := range routes {
		d := path.Dir(r.Node.Path)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

func dedupSortedStrings(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		if !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	sort.Strings(out)
	return out
}

func formatRootList(roots []string, candidateDirs map[string]bool) string {
	parts := make([]string, len(roots))
	for i, r := range roots {
		if candidateDirs[r] {
			parts[i] = r + " (candidate-seeded)"
		} else {
			parts[i] = r
		}
	}
	return strings.Join(parts, ", ")
}

func writeReachabilityHeader(sb *strings.Builder, roots []string, candidateDirs map[string]bool, rootNote string) {
	fmt.Fprintf(sb, "topology reachability: %s\n", reachabilityConfidenceLine)
	fmt.Fprintf(sb, "roots (%d): %s%s\n\n", len(roots), formatRootList(roots, candidateDirs), rootNote)
}

// formatReachabilitySummary is the default shape: reachable/unreachable
// package counts, each with up to reachabilitySampleLimit samples.
// Unreachable is sorted by size descending — the biggest dead package is the
// most actionable one to notice.
func formatReachabilitySummary(g *topology.PackageGraph, res *topology.ReachabilityResult, candidateDirs map[string]bool, rootNote string) string {
	var sb strings.Builder
	writeReachabilityHeader(&sb, res.Roots, candidateDirs, rootNote)

	reached := make([]string, 0, len(res.Reachable))
	for d := range res.Reachable {
		reached = append(reached, d)
	}
	sort.Strings(reached)
	fmt.Fprintf(&sb, "reachable: %d package(s)\n", len(reached))
	shown := reached
	if len(shown) > reachabilitySampleLimit {
		shown = shown[:reachabilitySampleLimit]
	}
	for _, d := range shown {
		fmt.Fprintf(&sb, "  %s\n", d)
	}
	if len(reached) > len(shown) {
		fmt.Fprintf(&sb, "  [+%d more]\n", len(reached)-len(shown))
	}
	sb.WriteString("\n")

	unreached := g.Unreachable(res.Reachable)
	fmt.Fprintf(&sb, "unreachable: %d package(s) (sorted by size — the actionable ones; a package used only by tests appears here by design — confirm before deleting)\n", len(unreached))
	ushown := unreached
	if len(ushown) > reachabilitySampleLimit {
		ushown = ushown[:reachabilitySampleLimit]
	}
	for _, info := range ushown {
		fmt.Fprintf(&sb, "  %s (%d node(s))\n", info.Dir, info.NumNodes)
	}
	if len(unreached) > len(ushown) {
		fmt.Fprintf(&sb, "  [+%d more]\n", len(unreached)-len(ushown))
	}

	return capReachabilityBytes(sb.String())
}

// formatReachabilityPath returns the single shortest root -> target directory
// chain, or a clear no-path answer — an unreached target is a legitimate
// finding, never an error.
func formatReachabilityPath(g *topology.PackageGraph, res *topology.ReachabilityResult, pathTo string, candidateDirs map[string]bool, rootNote string) string {
	var sb strings.Builder
	writeReachabilityHeader(&sb, res.Roots, candidateDirs, rootNote)

	target, ok := g.ResolveDir(pathTo)
	if !ok {
		fmt.Fprintf(&sb, "path_to %q: not an indexed package directory\n", pathTo)
		return capReachabilityBytes(sb.String())
	}
	chain, found := topology.PathTo(res, target)
	if !found {
		fmt.Fprintf(&sb, "path to %s: no path — unreachable from the given roots\n", target)
		return capReachabilityBytes(sb.String())
	}
	fmt.Fprintf(&sb, "path to %s:\n  %s\n", target, strings.Join(chain, " -> "))
	return capReachabilityBytes(sb.String())
}

// formatReachabilityLayers returns the package-SCC condensation of the
// reachable subgraph as topological layers. A component with more than one
// package IS the finding — it is flagged [cycle] rather than filtered.
func formatReachabilityLayers(g *topology.PackageGraph, res *topology.ReachabilityResult, candidateDirs map[string]bool, rootNote string) string {
	var sb strings.Builder
	writeReachabilityHeader(&sb, res.Roots, candidateDirs, rootNote)

	sccs := topology.CondenseSCCs(g, res.Reachable)
	cycles := 0
	for _, s := range sccs {
		if s.Cycle {
			cycles++
		}
	}
	fmt.Fprintf(&sb, "layers: %d SCC(s) over %d reachable package(s), %d flagged as cycles\n", len(sccs), len(res.Reachable), cycles)

	curLayer := -1
	shown := 0
	for _, s := range sccs {
		if shown >= reachabilityLayerLimit {
			fmt.Fprintf(&sb, "  [+%d more SCC(s)]\n", len(sccs)-shown)
			break
		}
		if s.Layer != curLayer {
			curLayer = s.Layer
			fmt.Fprintf(&sb, "  layer %d:\n", curLayer)
		}
		label := ""
		if s.Cycle {
			label = "  [cycle]"
		}
		fmt.Fprintf(&sb, "    %s%s\n", strings.Join(s.Packages, ", "), label)
		shown++
	}

	return capReachabilityBytes(sb.String())
}

// capReachabilityBytes is the hard backstop under reachabilityMaxBytes,
// truncating on a line boundary and saying so. The per-shape sample caps
// above are what actually shape a normal answer; this only fires on a
// pathologically wide result.
func capReachabilityBytes(s string) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= reachabilityMaxBytes {
		return s
	}
	cut := s[:reachabilityMaxBytes]
	if idx := strings.LastIndexByte(cut, '\n'); idx > 0 {
		cut = cut[:idx]
	}
	return cut + "\n  [response truncated to fit the byte cap]"
}
