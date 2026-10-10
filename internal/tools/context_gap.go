package tools

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_gap.go — what the call graph cannot say, said out loud.
//
// The call resolver follows package-qualified calls only. A call on a receiver
// (`c.Total()`) carries no type information in a syntactic parse and is left
// unresolved, so a method's caller list is short by an unknown amount and an empty
// one proves nothing. The only route from a method to such a caller is the import
// graph: a package that imports the method's package may call it. Those
// importers' declarations are surfaced as gap candidates at evidence 0, labelled
// as candidates, and never as callers (C06).

// Caps for gap candidates.
const (
	contextGapSubjects   = 8 // callables whose importers are searched
	contextGapPerSubject = 5 // candidates one subject contributes
	contextGapQuota      = 8 // candidates a pack lists, outside the ranked node cap
)

// The labels the cases require. They are spelled once so the renderer, the tests
// and the oracle cannot drift apart.
const (
	labelGoCallGraph  = "call graph is Go-only and syntactic; receiver-method callers may be absent"
	labelNoProof      = "receiver-method call sites unresolved: an empty or short caller list is not proof of no callers"
	labelMembers      = "struct fields are not graph members (disclosed, not claimed absent)"
	labelIndexFailing = "index unavailable or failing: no relationship or test-impact claims; body from a disk snapshot"
	labelStructural   = "structural only: relationships come from the topology index; no language server was consulted"
	labelStale        = "file changed since indexing: relationships may be stale"
)

func callableKind(k topology.NodeKind) bool {
	return k == topology.KindFunction || k == topology.KindMethod
}

// gapSubject is a callable whose callers the call graph may not fully show.
type gapSubject struct {
	hopNode
	callerFiles map[string]bool // files that hold a resolved caller of it
}

// gapSubjects are the callable symbol seeds and the resolved callers of those
// seeds, in a fixed order and capped. Only subjects the call-graph admission rule
// accepts qualify: for any other language there is no import-resolved graph to
// reason from, and the language label says so instead.
func (e *expander) gapSubjects(ctx context.Context) []gapSubject {
	callerFiles := map[int64]map[string]bool{}
	var callers []contextRelated
	seedIDs := map[int64]bool{}
	for _, s := range e.seeds {
		seedIDs[s.node.ID] = true
	}
	for _, r := range e.kept {
		if r.CallerOf == 0 {
			continue
		}
		if callerFiles[r.CallerOf] == nil {
			callerFiles[r.CallerOf] = map[string]bool{}
		}
		callerFiles[r.CallerOf][r.Node.Path] = true
		if seedIDs[r.CallerOf] {
			callers = append(callers, r)
		}
	}
	slices.SortFunc(callers, compareRelated)

	var subs []gapSubject
	add := func(h hopNode) {
		if len(subs) < contextGapSubjects && callableKind(h.node.Kind) && e.derivedAdmitted(ctx, h.node.ID) {
			subs = append(subs, gapSubject{hopNode: h, callerFiles: callerFiles[h.node.ID]})
		}
	}
	for _, s := range e.seeds {
		add(s)
	}
	for _, r := range callers {
		add(hopNode{node: r.Node, dist: r.Dist, sel: nodeSelector(r.Node)})
	}
	return subs
}

// gapCandidates lists, for each subject, the declarations of the files that import
// its package, ranked and capped to the gap quota.
func (e *expander) gapCandidates(ctx context.Context) []contextRelated {
	var out []contextRelated
	seen := map[string]bool{}
	for _, sub := range e.gapSubjects(ctx) {
		if ctx.Err() != nil {
			e.stats.Deadline = true
			break
		}
		out = append(out, e.gapFor(ctx, sub, seen)...)
	}
	slices.SortFunc(out, compareRelated)
	if len(out) > contextGapQuota {
		e.stats.GapMore += len(out) - contextGapQuota
		out = out[:contextGapQuota]
	}
	return out
}

// gapFor is one subject's candidates: callable declarations in the files that
// import its package, minus files that already hold a resolved caller (those are
// not a gap) and minus anything the pack already lists as a relationship.
func (e *expander) gapFor(ctx context.Context, sub gapSubject, seen map[string]bool) []contextRelated {
	pkg, ok := e.packageOf(ctx, sub.node.Path)
	if !ok {
		return nil
	}
	var out []contextRelated
	for _, f := range e.importerFiles(ctx, sub, pkg) {
		for _, d := range e.declsOf(ctx, f) {
			key := relatedIdentity(d)
			if !callableKind(d.Kind) || seen[key] || e.idents[key] {
				continue
			}
			seen[key] = true
			r := e.makeRelated(d, sub.hopNode, evidenceGap, "", 0,
				fmt.Sprintf("imports package %s, home of %s", pkg.Name, sub.sel))
			r.Gap, r.GapFor = true, sub.node.ID
			out = append(out, r)
		}
	}
	if len(out) > contextGapPerSubject {
		e.stats.GapMore += len(out) - contextGapPerSubject
		out = out[:contextGapPerSubject]
	}
	return out
}

// packageOf is the package clause node of a file, which the import resolver's
// edges point at.
func (e *expander) packageOf(ctx context.Context, p string) (topology.Node, bool) {
	for _, n := range e.nodesOf(ctx, p) {
		if n.Kind == topology.KindPackage {
			return n, true
		}
	}
	return topology.Node{}, false
}

// importerFiles are the production files outside the subject's directory that
// import its package. Each importer is checked against the scope before it is
// listed, the same as every other hop.
func (e *expander) importerFiles(ctx context.Context, sub gapSubject, pkg topology.Node) []string {
	res, err := e.store.ImpactFrom(ctx, pkg, topology.ImpactOpts{
		Depth: 1, MaxNodes: contextHopNodes, MaxBytes: topology.MaxTraversalBytes(), EdgeKinds: []string{"imports"},
	})
	if err != nil || res.DependedOnBy == nil {
		if err != nil {
			e.fail(ctx, err)
		}
		return nil
	}
	dir := path.Dir(sub.node.Path)
	var files []string
	for _, n := range res.DependedOnBy.Nodes {
		if n.Kind != topology.KindImport || path.Dir(n.Path) == dir || isTestPath(n.Path) || sub.callerFiles[n.Path] {
			continue
		}
		if e.allowedPath(n.Path) {
			files = append(files, n.Path)
		}
	}
	slices.Sort(files)
	return slices.Compact(files)
}

// coverageGap says which languages the index holds files for but cannot parse,
// when the caller seeded one of them: its symbols and relationships are unknown,
// not absent. The census is a few COUNTs, so it is read only when it is needed.
func coverageGap(store *topology.Store, seeds []contextSeed) []string {
	if store == nil || !slices.ContainsFunc(seeds, func(s contextSeed) bool { return s.Coverage != "" }) {
		return nil
	}
	uncovered := store.Status().UncoveredFiles
	if len(uncovered) == 0 {
		return nil
	}
	var parts []string
	for _, lang := range slices.Sorted(maps.Keys(uncovered)) {
		parts = append(parts, fmt.Sprintf("%s (%d)", lang, uncovered[lang]))
	}
	return []string{"coverage gap: the index holds files it has no extractor for (" + strings.Join(parts, ", ") +
		"); their symbols and relationships are unknown, not absent"}
}

// callerNote is what a callable seed's resolved caller list does and does not say.
type callerNote struct {
	Selector string
	Resolved int
}

// languageNote is what the call graph covers for one language in the pack.
type languageNote struct {
	Language  string
	Admitted  bool // function-level cross-file call edges exist for it
	Heuristic bool // its relationships here are heuristic edges
}

// contextExpansion is the expansion's record on the pack: whether it ran, what it
// could not do, and the disclosures the gaps section builds from it.
type contextExpansion struct {
	Ran       bool
	Stats     expansionStats
	Callers   []callerNote
	Languages []languageNote
}

// finish adds the gap candidates, applies the node cap and returns the pool in
// rank order, with the gap candidates after it.
func (e *expander) finish(ctx context.Context) []contextRelated {
	gaps := e.gapCandidates(ctx)
	pool := slices.Clone(e.kept)
	slices.SortFunc(pool, compareRelated)
	if room := contextMaxExpansionNodes - len(gaps); len(pool) > room {
		e.stats.Truncated += len(pool) - room
		pool = pool[:room]
	}
	return append(pool, gaps...)
}

// summarise records the expansion on the pack's side of the contract.
func (e *expander) summarise(ctx context.Context, related []contextRelated) contextExpansion {
	x := contextExpansion{Ran: true, Stats: e.stats}
	langs := map[string]topology.Node{}
	heuristic := map[string]bool{}
	note := func(n topology.Node) {
		if n.Language != "" && n.Language != "markdown" {
			if _, ok := langs[n.Language]; !ok {
				langs[n.Language] = n
			}
		}
	}
	for _, s := range e.seeds {
		note(s.node)
		if callableKind(s.node.Kind) {
			x.Callers = append(x.Callers, e.callerNote(s, related))
		}
	}
	for _, r := range related {
		note(r.Node)
		if r.Source == sourceHeuristic {
			heuristic[r.Node.Language] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(langs)) {
		x.Languages = append(x.Languages, languageNote{
			Language: name, Admitted: e.derivedAdmitted(ctx, langs[name].ID), Heuristic: heuristic[name],
		})
	}
	return x
}

func (e *expander) callerNote(s hopNode, related []contextRelated) callerNote {
	n := 0
	for _, r := range related {
		if r.CallerOf == s.node.ID {
			n++
		}
	}
	return callerNote{Selector: s.sel, Resolved: n}
}

// relationGaps is the disclosure text for the expansion, in the order a reader
// needs it: what kind of evidence this is, what the call graph cannot see, and
// what the caps cut. It says what is absent; it never says there is nothing.
func (p *contextPack) relationGaps() []string {
	x := p.Expansion
	if !x.Ran {
		return nil
	}
	gaps := []string{labelStructural}
	for _, l := range x.Languages {
		if l.Admitted {
			gaps = append(gaps, labelGoCallGraph+"; "+labelNoProof)
			continue
		}
		gaps = append(gaps, languageGap(l))
	}
	if slices.ContainsFunc(p.Files, func(f bodyFile) bool { return f.Changed }) {
		gaps = append(gaps, labelStale)
	}
	for _, c := range x.Callers {
		gaps = append(gaps, c.text(x.Stats.capped()))
	}
	if x.Stats.MembersListed {
		gaps = append(gaps, labelMembers)
	}
	return append(gaps, x.Stats.gaps()...)
}

func languageGap(l languageNote) string {
	kind := ""
	suffix := ""
	if l.Heuristic {
		kind, suffix = "heuristic ", " (confidence 0.8)"
	}
	return fmt.Sprintf("cross-file call graph unavailable for %s; intra-file %scall edges only%s", l.Language, kind, suffix)
}

// text words one seed's caller list. A list that is empty, short or cut is never
// "no callers".
func (c callerNote) text(capped bool) string {
	switch {
	case c.Resolved == 0:
		return fmt.Sprintf("callers of %s: none resolved in the index; this is not proof of no callers (gap candidates, if any, are listed separately)", c.Selector)
	case capped:
		return fmt.Sprintf("callers of %s: %d resolved and listed; the list may be incomplete (a cap or the deadline cut the walk)", c.Selector, c.Resolved)
	}
	return fmt.Sprintf("callers of %s: %d resolved and listed; the call graph is syntactic, so more may exist", c.Selector, c.Resolved)
}

// capped reports that a cap, the deadline or an error may have cut the walk.
func (s expansionStats) capped() bool {
	return s.Deadline || s.Failed != "" || s.HopCapped > 0 || s.Truncated > 0
}

// gaps words what the caps and the scope cut.
func (s expansionStats) gaps() []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if s.Deadline {
		add("expansion stopped at the %s deadline: the related list is partial", contextExpansionDeadline)
	}
	if s.Failed != "" {
		add("expansion met an index error (%s): the related list is partial", s.Failed)
	}
	if s.Truncated > 0 {
		add("%d further candidate(s) beyond the %d-node, depth-%d cap were not kept; the frozen ranking decided which", s.Truncated, contextMaxExpansionNodes, contextExpansionDepth)
	}
	if s.HopCapped > 0 {
		add("%d neighbour list(s) exceeded %d and were cut; callers or callees may be missing", s.HopCapped, contextHopNodes)
	}
	if s.MembersCapped {
		add("a type has more than %d members; the member list is capped", topology.MemberListCap)
	}
	if s.GapMore > 0 {
		add("%d further gap candidate(s) beyond their quota are not listed", s.GapMore)
	}
	if s.Excluded > 0 {
		add("%d file(s) reached from the seeds are outside within/corpora and were left out; nothing was expanded through them", s.Excluded)
	}
	return out
}
