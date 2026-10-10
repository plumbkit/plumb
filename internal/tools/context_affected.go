package tools

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"slices"

	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/topology"
)

// context_affected.go — the compact test-impact estimate.
//
// This is topology_affected's own gather (collectAffected) run over the pack's
// seeds, kept to one row per test package: the directory (or the run_task target
// the calling agent's test command accepts), how many tests live there and why the
// package is implicated. It names no individual test, and it runs none (Invariant
// 10): every word here says "estimate" and "implicated", never "passed" or "ran".
//
// The estimate rests on the index, so a missing, failing or foreign index gives
// none at all (the index gap already says so), and the scope filter applies to
// what the pack names: a test outside within/corpora is left out and counted.

const (
	// contextAffectedPackages bounds the package rows gathered. The packer decides
	// how many of them the budget shows; this only bounds the work.
	contextAffectedPackages = 16
	// contextAffectedNeighbours bounds the neighbouring declarations a change may
	// extend into that the estimate also starts from.
	contextAffectedNeighbours = 8
)

// contextAffected is the estimate and what bounded it.
type contextAffected struct {
	Ran       bool
	Packages  []affectedPackage
	Scope     TestScope
	Neighbour map[string]bool // directories implicated only through a neighbour of a seed
	Excluded  int             // tests left out by within, corpora or plumb's own state
	Cut       bool            // more packages than the cap
	GraphCut  bool            // the dependent walk hit its traversal budget
	Deadline  bool            // the graph deadline passed, so the estimate is partial
}

// affectedTests gathers the estimate. ctx carries the call's graph deadline.
func (c *ContextCollector) affectedTests(ctx context.Context, pack *contextPack, scope contextScope, index contextIndex) {
	if len(pack.Seeds) == 0 || !index.usable() {
		return
	}
	roots, dirs := affectedRoots(ctx, index.store, pack.Seeds)
	if len(roots)+len(dirs) == 0 {
		return // no code seed: nothing to estimate from, and nothing to disclaim
	}
	near := affectedNeighbours(pack)
	res, err := collectAffected(ctx, index.store, append(roots, near...), dirs, contextAffectedPackages)
	if err != nil || res == nil {
		return
	}
	kept, excluded := scopeAffected(res.Tests, scope)
	nearDirs := neighbourDirs(roots, dirs, near)
	pack.Affected = contextAffected{
		Ran: true, Packages: seedPackagesFirst(aggregateTestsByPackage(kept), nearDirs), Excluded: excluded, Neighbour: nearDirs,
		Cut: res.Truncated, GraphCut: res.GraphTruncated, Deadline: ctx.Err() != nil,
	}
	for i := range pack.Affected.Packages {
		pack.Affected.Packages[i].Tests = nil // the estimate names packages, never tests
	}
	if c.testScope != nil {
		pack.Affected.Scope = c.testScope(ctx)
	}
}

// affectedRoots lists the code symbols the estimate starts from, and the
// directories whose co-located tests it may add. A symbol seed starts from its own
// node, a covered code file from its declarations; a file the index cannot parse
// still contributes its directory, as topology_affected does for any changed file.
func affectedRoots(ctx context.Context, store *topology.Store, seeds []contextSeed) (roots []topology.Node, dirs []string) {
	for _, s := range seeds {
		if corpusOfPath(s.Path) != corpusCode {
			continue
		}
		if s.Kind == seedSymbol {
			roots = append(roots, s.node())
			continue
		}
		dirs = append(dirs, path.Dir(s.Path))
		if s.Coverage != "" {
			continue
		}
		if nodes, err := store.SymbolsInFile(ctx, s.Path); err == nil {
			roots = append(roots, declarationRoots(nodes)...)
		}
	}
	return roots, dirs
}

// affectedNeighbours are the declarations next to the seeds (one resolved edge
// away) that a change to the seeds may extend into, so their tests are implicated
// too. They matter only for intent=change: understanding code changes nothing.
// Each already passed the scope filter in the walk.
func affectedNeighbours(p *contextPack) []topology.Node {
	if p.Intent != contextIntentChange {
		return nil
	}
	var out []topology.Node
	for _, r := range p.Related {
		if len(out) == contextAffectedNeighbours {
			break
		}
		if r.Dist == 1 && !r.Gap && !r.Withheld && r.Evidence >= evidenceDerived && !isTestNode(r.Node) {
			out = append(out, r.Node)
		}
	}
	return out
}

// neighbourDirs are the directories only a neighbour of a seed puts in play: not
// the directory of a seed, and not one a seed file was named in.
func neighbourDirs(roots []topology.Node, seedDirs []string, near []topology.Node) map[string]bool {
	own := map[string]bool{}
	for _, d := range seedDirs {
		own[d] = true
	}
	for _, n := range roots {
		own[path.Dir(n.Path)] = true
	}
	out := map[string]bool{}
	for _, n := range near {
		if d := path.Dir(n.Path); !own[d] {
			out[d] = true
		}
	}
	return out
}

// seedPackagesFirst keeps topology_affected's order but puts the packages that
// hold a seed ahead of those only a neighbour of a seed puts in play.
func seedPackagesFirst(pkgs []affectedPackage, near map[string]bool) []affectedPackage {
	slices.SortStableFunc(pkgs, func(a, b affectedPackage) int {
		return cmp.Compare(boolInt(near[a.Dir]), boolInt(near[b.Dir]))
	})
	return pkgs
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// scopeAffected drops the tests the pack may not name and counts them.
func scopeAffected(tests []affectedTest, scope contextScope) (kept []affectedTest, excluded int) {
	for _, t := range tests {
		if ok, _ := scope.allows(t.Node.Path, corpusCode); !ok || plumbStateReason(t.Node.Path) != "" {
			excluded++
			continue
		}
		kept = append(kept, t)
	}
	return kept, excluded
}

const affectedHeading = "affected tests (static estimate from the index; no test was run or chosen for you):"

// affectedReasonWords rewords topology_affected's reasons for a read-only task:
// nothing is being changed here, so a package is implicated by a seed, not changed.
func affectedReasonWords(reason, dir string, a contextAffected) string {
	switch reason {
	case reasonChanged:
		if a.Neighbour[dir] {
			return "holds a declaration next to a seed"
		}
		return "holds a seed"
	case reasonImporter:
		return "imports a seed's package"
	case reasonGraph:
		return "reached by a dependency edge"
	}
	return reason
}

// affectedLines renders one row per test package, in topology_affected's order
// (the seeds' own packages first, then by test count).
func (p *contextPack) affectedLines() []packLine {
	if !p.Affected.Ran || len(p.Affected.Packages) == 0 {
		return nil
	}
	ls := []packLine{{text: affectedHeading, prio: prioAffected, section: secAffected, heading: true, class: classAffected}}
	for _, pk := range p.Affected.Packages {
		text := fmt.Sprintf("  %s — %d test(s); %s", textfmt.TerminalSafeLine(packageRunLabel(p.Affected.Scope, pk.Dir)),
			pk.Count, affectedReasonWords(pk.Reason, pk.Dir, p.Affected))
		ls = append(ls, packLine{text: text, prio: prioAffected, section: secAffected, class: classAffected})
	}
	return ls
}

// affectedGaps words what bounded the estimate. An estimate with no test in it is
// not proof that nothing exercises the seeds.
func (p *contextPack) affectedGaps() []string {
	a := p.Affected
	if !a.Ran {
		return nil
	}
	var gaps []string
	if len(a.Packages) == 0 && !a.Deadline {
		gaps = append(gaps, "affected tests: the index lists none for these seeds; that is not proof that nothing exercises them")
	}
	if a.Excluded > 0 {
		gaps = append(gaps, fmt.Sprintf("affected tests: %d test(s) outside within/corpora were left out of the estimate", a.Excluded))
	}
	if a.Cut {
		gaps = append(gaps, fmt.Sprintf("affected tests: the package list was cut at %d; more packages are implicated than are listed", contextAffectedPackages))
	}
	if a.GraphCut {
		gaps = append(gaps, "affected tests: the dependent walk hit its traversal budget, so packages reached only through the dropped part of the graph are not listed")
	}
	if a.Deadline {
		gaps = append(gaps, fmt.Sprintf("affected tests: the %s graph deadline passed, so the estimate is partial", contextExpansionDeadline))
	}
	return gaps
}
