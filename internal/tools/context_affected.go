package tools

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/topology"
)

// context_affected.go — the compact test-impact estimate.
//
// This is topology_affected's own gather (collectAffected) run over the pack's
// seeds, kept to one row per test package: the directory (or the run_task target
// the calling agent's test command accepts), how many tests live there and why the
// package is implicated. It runs none (Invariant 10): every word here says
// "estimate" and "implicated", never "passed" or "ran".
//
// Under a package row it may name a few tests, but only the ones a call edge
// reaches: a test that calls a seed, or a declaration next to one, directly or
// through one caller. Co-location says only that a test sits in the package, so a
// co-located test is counted, never named. With a Go test target the names come as
// a ready run_task filter, which is the step agents otherwise spell by hand before
// their first test run (PLAN-543: every arm D agent built its own -run filter).
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
	// contextNamedPerPackage and contextNamedPackages bound the named tests: past a
	// handful the line stops being a first run and becomes the package again.
	contextNamedPerPackage = 8
	contextNamedPackages   = 3
)

// contextAffected is the estimate and what bounded it.
type contextAffected struct {
	Ran       bool
	Packages  []affectedPackage
	Scope     TestScope
	Neighbour map[string]bool       // directories implicated only through a neighbour of a seed
	Named     map[string]namedTests // per package directory: the tests a call edge reaches
	Excluded  int                   // tests left out by within, corpora or plumb's own state
	Cut       bool                  // more packages than the cap
	GraphCut  bool                  // the dependent walk hit its traversal budget
	Deadline  bool                  // the graph deadline passed, so the estimate is partial
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
	near, aside := affectedNeighbours(pack)
	res, err := collectAffectedNear(ctx, index.store, roots, near, aside, dirs, contextAffectedPackages)
	if err != nil || res == nil {
		return
	}
	kept, excluded := scopeAffected(res.Tests, scope)
	nearDirs := neighbourDirs(roots, dirs, near)
	pack.Affected = contextAffected{
		Ran: true, Packages: seedPackagesFirst(aggregateTestsByPackage(kept), nearDirs), Excluded: excluded, Neighbour: nearDirs,
		Cut: res.Truncated, GraphCut: res.GraphTruncated, Deadline: ctx.Err() != nil,
	}
	pack.Affected.Named = edgeReachedTests(pack.Affected.Packages, indexFreshness(index.store.Health()))
	for i := range pack.Affected.Packages {
		pack.Affected.Packages[i].Tests = nil // only edgeReachedTests' names survive
	}
	if c.testScope != nil {
		pack.Affected.Scope = c.testScope(ctx)
	}
}

// namedTests are the tests of one package that a call edge reaches, those that
// exercise a seed first and the closest first among them (fewest hops, then
// name), and how many there were.
type namedTests struct {
	Names []string
	Total int
}

// edgeReachedTests names, per package, the tests the dependent walk reached by
// an edge rather than by sitting in an implicated directory. Only test functions
// go test -run can select are named; the first contextNamedPackages packages that
// have any are kept, in the rows' order.
//
// freshness is the index's (indexFreshness). A stale graph may name a test that
// no longer reaches the seed, and a wrong name costs the agent a run, so a stale
// index names none; its gap already says it is stale, and the package rows stay.
func edgeReachedTests(pkgs []affectedPackage, freshness string) map[string]namedTests {
	out := map[string]namedTests{}
	if freshness == "stale" {
		return out
	}
	for _, pk := range pkgs {
		if len(out) == contextNamedPackages {
			break
		}
		var reached []affectedTest
		seen := map[string]bool{}
		for _, t := range pk.Tests {
			if t.Confidence <= colocatedConfidence || !runnableTestName(t.Node.Name) || seen[t.Node.Name] {
				continue
			}
			seen[t.Node.Name] = true
			reached = append(reached, t)
		}
		if len(reached) == 0 {
			continue
		}
		// A test that exercises a seed comes before one that only reaches a callee or
		// sibling of it; then fewest hops, where a test whose connecting edge was
		// filtered out of the subgraph has no hop count (0) and goes last. Name breaks
		// ties, so the line is the same on every call.
		slices.SortFunc(reached, func(a, b affectedTest) int {
			if c := cmp.Compare(boolInt(a.Aside), boolInt(b.Aside)); c != 0 {
				return c
			}
			if c := cmp.Compare(hopRank(a.Hops), hopRank(b.Hops)); c != 0 {
				return c
			}
			return cmp.Compare(a.Node.Name, b.Node.Name)
		})
		n := namedTests{Total: len(reached)}
		for _, t := range reached[:min(len(reached), contextNamedPerPackage)] {
			n.Names = append(n.Names, t.Node.Name)
		}
		out[pk.Dir] = n
	}
	return out
}

// hopRank orders a known hop count before an unknown one (0).
func hopRank(h int) int {
	if h == 0 {
		return math.MaxInt
	}
	return h
}

// runnableTestName is a top-level function go test -run selects: Test, Example or
// Fuzz followed by nothing or a non-lowercase rune, as the testing package
// requires. A benchmark needs -bench, and a helper is not a test at all.
func runnableTestName(name string) bool {
	for _, p := range []string{"Test", "Example", "Fuzz"} {
		if rest, ok := strings.CutPrefix(name, p); ok {
			return rest == "" || !unicode.IsLower([]rune(rest)[0])
		}
	}
	return false
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
//
// aside holds the neighbours that are not callers of a seed (its callees and
// siblings): a test of one of those does not run the seed, so it ranks after
// every test that does. A caller's tests do run the seed, one hop further out.
//
// near is in walk order, callers first and then by name, whatever the ranking
// chose them in: a path that does not run the seed is then never recorded
// before one that does, and the same pack walks in the same order every time.
func affectedNeighbours(p *contextPack) (near []topology.Node, aside map[int64]bool) {
	if p.Intent != contextIntentChange {
		return nil, nil
	}
	aside = map[int64]bool{}
	for _, r := range p.Related {
		if len(near) == contextAffectedNeighbours {
			break
		}
		if r.Dist == 1 && !r.Gap && !r.Withheld && r.Evidence >= evidenceDerived && !isTestNode(r.Node) {
			near = append(near, r.Node)
			if r.CallerOf == 0 {
				aside[r.Node.ID] = true
			}
		}
	}
	slices.SortStableFunc(near, func(a, b topology.Node) int {
		if c := cmp.Compare(boolInt(aside[a.ID]), boolInt(aside[b.ID])); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return near, aside
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
		if named, ok := p.Affected.Named[pk.Dir]; ok {
			ls = append(ls, packLine{text: namedTestsLine(p.Affected.Scope, pk.Dir, named), prio: prioAffected, section: secAffected, class: classAffected})
		}
	}
	return ls
}

// namedTestsReach is how a named test is connected to the seeds. It is the walk's
// own claim and no more: the edges are the index's, and nothing was run.
const namedTestsReach = "call a seed or a declaration next to one, directly or through one caller (index call edges)"

// namedTestsLine renders one package's edge-reached tests. With a Go package
// target it is a run_task call ready to paste, its filter cut to the names that
// fit the filter's length limit; otherwise it is the names. The line never says
// the tests ran or passed.
func namedTestsLine(scope TestScope, dir string, n namedTests) string {
	names := n.Names
	if target, ok := testTarget(scope, dir); ok && scope.Style == TargetGoPackage {
		for len(names) > 0 && validateRunFilter("run", runFilterFor(names)) != nil {
			names = names[:len(names)-1]
		}
		if len(names) > 0 {
			return fmt.Sprintf("    run first: run_task(slot:\"test\", target:%q, run:%q) — %s %s%s",
				target, runFilterFor(names), namedCount(len(names), n.Total), namedTestsReach, namedMore(len(names), n.Total))
		}
		names = n.Names
	}
	return fmt.Sprintf("    %s %s: %s%s", namedCount(len(names), n.Total), namedTestsReach,
		textfmt.TerminalSafeLine(strings.Join(names, ", ")), namedMore(len(names), n.Total))
}

// runFilterFor anchors the names as one group: go test -run splits a pattern only
// at a top-level |, so the alternation inside the parentheses stays whole. The
// names' common prefix is written once (^TestMoveSymbol_(Refusals|CreateDestination)$),
// which fits more of them in run_task's filter limit. Test names are identifiers,
// so nothing in them needs escaping.
func runFilterFor(names []string) string {
	if len(names) == 1 {
		return "^(" + names[0] + ")$"
	}
	prefix := names[0]
	for _, n := range names[1:] {
		for !strings.HasPrefix(n, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	for !utf8.ValidString(prefix) { // never split a rune between prefix and rest
		prefix = prefix[:len(prefix)-1]
	}
	rests := make([]string, len(names))
	for i, n := range names {
		rests[i] = n[len(prefix):]
	}
	return "^" + prefix + "(" + strings.Join(rests, "|") + ")$"
}

func namedCount(shown, total int) string {
	if shown == total {
		return fmt.Sprintf("%d test(s)", total)
	}
	return fmt.Sprintf("%d of %d test(s) that", shown, total)
}

func namedMore(shown, total int) string {
	if shown == total {
		return ""
	}
	return fmt.Sprintf("; %d more are not named", total-shown)
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
