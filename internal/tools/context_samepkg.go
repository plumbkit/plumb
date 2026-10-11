package tools

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"maps"
	"os"
	"path"
	"slices"
	"sync"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_samepkg.go — callers the call graph cannot resolve, found by name in the
// same Go package (PLAN-462 gate v2, item 2).
//
// Gate v2 added this pass when the call resolver followed package-qualified calls
// only (about 3% of recorded call sites). The resolver now also links bare
// same-package calls and method calls on the caller's own receiver, a typed
// parameter, or a local variable its function declares once with an evident type
// (#632), so on an index that publishes that census the pass is skipped for
// functions (functionsResolved). It still runs for methods: its e1 matches of
// callers the resolver already links lose to the resolved e2 edge, and what is left
// is what the resolver leaves (a local declared twice, an index built before the
// tiers) plus the e0 gap candidates for receivers of no evident type. It runs for
// functions too on an index built before the census. The call sites themselves are
// recorded verbatim, with the declaration that holds them, so this collector-local
// pass reads them back and matches them by name:
//
//   - A function: a bare call of its name, in a file of the same directory and
//     package clause, where no local declaration of the caller shadows the name.
//   - A method: a call `x.M()` where the caller's own declarations give x the
//     method's receiver type. A receiver nothing identifies is not evidence.
//
// A match is admitted as a caller at evidence 1, labelled "same-package name match",
// never as a resolved edge. Where the name does not single out one declaration (two
// of that name in the package, a receiver of no evident type, a file withheld as
// sensitive) the site is only an evidence-0 gap candidate, the same class as a
// reverse-import candidate. Nothing here is a claim that a declaration has no
// callers. The scope filter runs before a file is read, a sensitive file is never
// read, and only the declaration's own package is searched, so nothing outside the
// agent's root or scope is reached.

// Limits of the same-package search.
const (
	// contextSamePkgPerSeed is the most same-package name-match callers one
	// declaration (a seed, or a neighbour the walk expands) contributes. It is the one
	// knob PLAN-462 gate v2 let one calibration pass on dev ids set: 3, 6 and 12 were
	// tried; dev end-to-end residual recall at 12k was 0/13 for all three, so the pick
	// is the best dev precision (12: 6974/16020 body bytes, against 6784/15830).
	contextSamePkgPerSeed = 12

	// contextSamePkgSiteScan bounds the call sites examined for one declaration, so a
	// common name cannot cost a parse of every file that uses it.
	contextSamePkgSiteScan = 64
	// contextSamePkgDirSites bounds the call sites one read of a directory returns.
	contextSamePkgDirSites = 200_000
	// contextSamePkgReaders is how many directories a level reads at once.
	contextSamePkgReaders = 4
	// contextSamePkgFileMax and contextSamePkgReadBytes bound the source one file, and
	// one call, may read for this pass.
	contextSamePkgFileMax   = 1 << 20
	contextSamePkgReadBytes = 4 << 20

	samePkgLanguage = "go"
)

const labelSamePackage = "same-package callers are call sites in sibling files of the package that name the declaration, " +
	"matched by name and not resolved by the call graph (e1, heuristic); a method needs receiver evidence in the caller " +
	"itself, otherwise it is only a gap candidate (e0)"

var errSamePkgUnreadable = errors.New("the file cannot be read for the same-package search")

// samePackage is one walk's same-package search. It reads source only through read,
// and trusts a file only if hash says it is the bytes the index parsed.
type samePackage struct {
	read    func(rel string) ([]byte, error) // guard-checked, root-keyed
	hash    func(rel string) string          // the content hash the index holds for rel
	perSeed int
	dirs    map[string]*dirSites
	files   map[string]*goFile // nil value: the file's sites cannot be examined
	budget  int64
	gaps    map[string]contextRelated // ambiguous candidates, by declaration identity
	// functionsResolved: the index resolves bare same-package calls itself
	// (ResolvesSamePackageCalls), so a function's sibling-file callers are already
	// derived edges and the name search would only find them again. A method still
	// gets the search: the resolver links only the calls whose receiver type the
	// declaration states.
	functionsResolved bool
}

// dirSites is the call sites read so far of one directory, by callee, and the names
// already asked for.
type dirSites struct {
	byCallee map[string][]topology.CallSiteRef
	asked    map[string]bool
}

// samePackageCap is the per-declaration cap this collector applies: the calibrated
// constant, unless a test has set its own.
func (c *ContextCollector) samePackageCap() int {
	if c.samePkgCap > 0 {
		return c.samePkgCap
	}
	return contextSamePkgPerSeed
}

func newSamePackage(read func(string) ([]byte, error), hash func(string) string, perSeed int) *samePackage {
	return &samePackage{
		read: read, hash: hash, perSeed: perSeed,
		dirs: map[string]*dirSites{}, files: map[string]*goFile{}, budget: contextSamePkgReadBytes,
		gaps: map[string]contextRelated{},
	}
}

// sourceReader is the collector's read of a sibling file for the same-package
// search: under the agent's canonical root only (a path that resolves outside it,
// through a symlink say, is refused), through the boundary guard, a regular file of
// bounded size.
func (c *ContextCollector) sourceReader(ctx context.Context, root string) func(string) ([]byte, error) {
	return func(rel string) ([]byte, error) {
		abs := absUnder(root, rel)
		if relWithinRoot(root, abs) == "" {
			return nil, errSamePkgUnreadable
		}
		if err := c.guard.check(ctx, abs); err != nil {
			return nil, err
		}
		info, err := os.Stat(abs)
		if err != nil || !info.Mode().IsRegular() || info.Size() > contextSamePkgFileMax {
			return nil, errSamePkgUnreadable
		}
		return os.ReadFile(abs)
	}
}

// samePkgSubject is the declaration whose same-package callers are sought.
type samePkgSubject struct {
	h         hopNode
	name      string
	dir, pkg  string
	method    bool
	recv      string // a method's receiver type, type parameters dropped
	ambiguous string // why the name does not single out one declaration; "" when it does
}

type siteVerdict int

const (
	siteSkip      siteVerdict = iota // not a caller, or not examinable
	siteCaller                       // evidence 1
	siteCandidate                    // evidence 0
)

// samePkgHit is a declaration found holding a matching call site.
type samePkgHit struct {
	node   topology.Node
	caller bool
	why    string // a candidate's reason
}

// samePackageCallers is the caller-by-name step of one hop: the e1 callers of h
// (capped per declaration), with its e0 candidates kept apart for the gap section.
func (e *expander) samePackageCallers(ctx context.Context, h hopNode) []contextRelated {
	// Only Go has call sites recorded for this search (CallSitesInDir), and only a
	// declaration with a package clause of its own has a package to search.
	if e.sp == nil || !callableKind(h.node.Kind) || ctx.Err() != nil {
		return nil
	}
	if e.sp.functionsResolved && h.node.Kind == topology.KindFunction {
		return nil
	}
	pkg, ok := e.packageOf(ctx, h.node.Path)
	if !ok {
		return nil
	}
	subj, ok := newSamePkgSubject(h, pkg.Name)
	if !ok {
		return nil
	}
	ds := e.siteList(ctx, subj.dir, []string{subj.name})
	sites := candidateSites(ds, subj)
	if len(sites) == 0 {
		return nil
	}
	subj.ambiguous = e.sameNameNote(ctx, subj)
	hits := e.judgeSites(ctx, subj, sites)
	callers, cands := e.samePkgRelated(subj, hits)
	e.admitSamePkgGaps(cands)
	return e.capSamePkg(callers)
}

func newSamePkgSubject(h hopNode, pkg string) (samePkgSubject, bool) {
	s := samePkgSubject{h: h, name: h.node.Name, dir: path.Dir(h.node.Path), pkg: pkg, method: h.node.Kind == topology.KindMethod}
	if s.method {
		recv, _, ok := goMethodReceiver(nodeSelector(h.node))
		if !ok {
			return s, false
		}
		s.recv = stripTypeParams(recv)
	}
	return s, s.name != ""
}

// candidateSites are the directory's call sites of the subject's name in files of
// its package, production code before tests, in a fixed order.
func candidateSites(ds *dirSites, s samePkgSubject) []topology.CallSiteRef {
	var out []topology.CallSiteRef
	for _, site := range ds.byCallee[s.name] {
		if site.Package == s.pkg {
			out = append(out, site)
		}
	}
	slices.SortStableFunc(out, func(a, b topology.CallSiteRef) int {
		return cmp.Compare(testRank(a.Path), testRank(b.Path))
	})
	return out
}

func testRank(p string) int {
	if isTestPath(p) {
		return 1
	}
	return 0
}

// siteRead is one directory's read of call sites still to be made.
type siteRead struct {
	dir       string
	ds        *dirSites
	names     []string
	sites     []topology.CallSiteRef
	truncated bool
	err       error
}

// planRead records names as asked of dir's list and returns the read still to make:
// nil when every name was asked before.
func (e *expander) planRead(dir string, names []string) *siteRead {
	ds := e.sp.dirs[dir]
	if ds == nil {
		ds = &dirSites{byCallee: map[string][]topology.CallSiteRef{}, asked: map[string]bool{}}
		e.sp.dirs[dir] = ds
	}
	r := &siteRead{dir: dir, ds: ds}
	for _, n := range names {
		if !ds.asked[n] {
			ds.asked[n] = true
			r.names = append(r.names, n)
		}
	}
	if len(r.names) == 0 {
		return nil
	}
	return r
}

// readSites makes the reads, directories side by side (the store answers reads
// concurrently, and a large directory takes tens of milliseconds), then folds them
// into their directories' lists in the order given, so the result does not depend on
// which finished first.
func (e *expander) readSites(ctx context.Context, reads []*siteRead) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, contextSamePkgReaders)
	for _, r := range reads {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			r.sites, r.truncated, r.err = e.store.CallSitesInDir(ctx, r.dir, samePkgLanguage, r.names, contextSamePkgDirSites)
		}()
	}
	wg.Wait()
	for _, r := range reads {
		if r.err != nil {
			e.fail(ctx, r.err)
			continue
		}
		if r.truncated {
			e.stats.SamePkgDirsCut++
		}
		for _, s := range r.sites {
			r.ds.byCallee[s.Callee] = append(r.ds.byCallee[s.Callee], s)
		}
	}
}

// siteList is a directory's recorded calls of the given names, each name read once.
// A directory records tens of thousands of call sites, so only the names a walk is
// about to ask for are read, and a whole level's together (prefetchSites).
func (e *expander) siteList(ctx context.Context, dir string, names []string) *dirSites {
	if r := e.planRead(dir, names); r != nil {
		e.readSites(ctx, []*siteRead{r})
	}
	return e.sp.dirs[dir]
}

// prefetchSites reads, for one level of the walk, the calls of every callable it is
// about to expand, one query per directory instead of one per declaration.
func (e *expander) prefetchSites(ctx context.Context, frontier []hopNode) {
	if e.sp == nil {
		return
	}
	byDir := map[string][]string{}
	for _, h := range frontier {
		if callableKind(h.node.Kind) {
			dir := path.Dir(h.node.Path)
			byDir[dir] = append(byDir[dir], h.node.Name)
		}
	}
	var reads []*siteRead
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		if r := e.planRead(dir, byDir[dir]); r != nil {
			reads = append(reads, r)
		}
	}
	e.readSites(ctx, reads)
}

// sameNameNote says why the subject's name does not single out one declaration of the
// package: a function counts against other functions of the name, a method against
// other methods of the name on the same receiver type (a method of another type is
// told apart by the call's receiver). "" when it is the only one.
func (e *expander) sameNameNote(ctx context.Context, s samePkgSubject) string {
	decls, err := e.store.DeclsNamedInDir(ctx, s.dir, samePkgLanguage, s.name)
	if err != nil {
		e.fail(ctx, err)
		return "the package's declarations of the name could not be listed"
	}
	n := 0
	for _, d := range decls {
		if node, ok := e.packageOf(ctx, d.Path); ok && node.Name == s.pkg && sameKindOfDecl(s, d) {
			n++
		}
	}
	if n > 1 {
		return fmt.Sprintf("%d declarations of %s in the package", n, s.name)
	}
	return ""
}

func sameKindOfDecl(s samePkgSubject, d topology.Node) bool {
	if !s.method {
		return d.Kind == topology.KindFunction
	}
	recv, _, ok := goMethodReceiver(nodeSelector(d))
	return d.Kind == topology.KindMethod && ok && stripTypeParams(recv) == s.recv
}

// judgeSites examines the candidate sites (up to the scan bound) and returns the
// declarations holding a match, keyed by node, a caller outranking a candidate.
func (e *expander) judgeSites(ctx context.Context, s samePkgSubject, sites []topology.CallSiteRef) map[int64]samePkgHit {
	hits := map[int64]samePkgHit{}
	for i, site := range sites {
		if i >= contextSamePkgSiteScan {
			e.stats.SamePkgCut += len(sites) - i
			break
		}
		if ctx.Err() != nil {
			e.stats.Deadline = true
			break
		}
		node, v, why := e.judgeSite(ctx, s, site)
		if v == siteSkip {
			continue
		}
		if old, seen := hits[node.ID]; seen && (old.caller || v != siteCaller) {
			continue
		}
		hits[node.ID] = samePkgHit{node: node, caller: v == siteCaller, why: why}
	}
	return hits
}

// judgeSite decides one site. The scope runs first, so a file outside it is never
// read; a sensitive file is named as a candidate and never read.
func (e *expander) judgeSite(ctx context.Context, s samePkgSubject, site topology.CallSiteRef) (topology.Node, siteVerdict, string) {
	if !e.allowedPath(site.Path) {
		return topology.Node{}, siteSkip, ""
	}
	encl, ok := e.enclosingOf(ctx, site)
	// A declaration the walk already holds is not found again: that includes the
	// subject itself (its recursion is no caller of it) and every seed. Its path was
	// judged by the scope filter above, and a declaration is never a reference.
	if !ok || e.idents[relatedIdentity(encl)] {
		return topology.Node{}, siteSkip, ""
	}
	if e.sensitive != nil && e.sensitive(site.Path) {
		return encl, siteCandidate, "its file is withheld, so the call was not checked"
	}
	g := e.goFile(site.Path)
	if g == nil {
		return topology.Node{}, siteSkip, ""
	}
	call, decl := g.callAt(site.StartByte, s.name), g.declAt(site.StartByte)
	if call == nil || decl == nil {
		return topology.Node{}, siteSkip, ""
	}
	v, why := s.judgeCall(g, call, decl)
	return encl, v, why
}

// judgeCall classifies a parsed call against the subject.
func (s samePkgSubject) judgeCall(g *goFile, call *ast.CallExpr, decl ast.Decl) (siteVerdict, string) {
	if !s.method {
		_, bare := unwrapCallee(call.Fun).(*ast.Ident)
		if !bare || len(localDecls(decl, s.name)) > 0 {
			return siteSkip, "" // a method or package call, or a local name shadows the function
		}
		return s.settled()
	}
	sel, ok := unwrapCallee(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return siteSkip, ""
	}
	typ, ev := g.receiverOf(sel.X, decl)
	switch {
	case ev == recvPackage || (ev == recvTyped && typ != s.recv):
		return siteSkip, "" // a package function, or another type's method
	case ev == recvTyped:
		return s.settled()
	}
	return siteCandidate, "the receiver's type is not evident"
}

// settled is the verdict for a call the caller's own declarations show is a call of
// the subject: a caller, unless the name does not single out one declaration.
func (s samePkgSubject) settled() (siteVerdict, string) {
	if s.ambiguous != "" {
		return siteCandidate, s.ambiguous
	}
	return siteCaller, ""
}

// enclosingOf is the declaration node that holds site, from the file's indexed
// nodes.
func (e *expander) enclosingOf(ctx context.Context, site topology.CallSiteRef) (topology.Node, bool) {
	for _, n := range e.nodesOf(ctx, site.Path) {
		if n.ID == site.EnclosingID {
			return n, true
		}
	}
	return topology.Node{}, false
}

// goFile parses a sibling file once, if it may be trusted: the index holds a hash
// for it, and the bytes read now hash to exactly that, so the byte offsets of its
// call sites mean what they meant when it was indexed. A file that changed since,
// cannot be read, or would pass the call's source budget is left unexamined, and
// counted.
func (e *expander) goFile(rel string) *goFile {
	sp := e.sp
	if g, ok := sp.files[rel]; ok {
		return g
	}
	g := sp.load(rel)
	sp.files[rel] = g
	if g == nil {
		e.stats.SamePkgSkipped++
	}
	return g
}

func (sp *samePackage) load(rel string) *goFile {
	src, err := sp.read(rel)
	if err != nil || int64(len(src)) > sp.budget {
		return nil
	}
	sp.budget -= int64(len(src))
	// An index that holds no hash for the file, or another one, vouches for nothing.
	if sum := sha256.Sum256(src); hex.EncodeToString(sum[:]) != sp.hash(rel) {
		return nil
	}
	g, _ := parseGoFile(rel, src)
	return g
}

// samePkgRelated builds the related entries for the hits: callers at evidence 1 and
// gap candidates at evidence 0.
func (e *expander) samePkgRelated(s samePkgSubject, hits map[int64]samePkgHit) (callers, cands []contextRelated) {
	for _, hit := range hits {
		test := ""
		if isTestPath(hit.node.Path) {
			test = ", test"
		}
		if hit.caller {
			r := e.makeRelated(hit.node, s.h, evidenceHeuristic, "", 0,
				fmt.Sprintf("caller of %s (same-package name match%s)", s.h.sel, test))
			r.Edge, r.NameMatch = topology.EdgeCalls, true
			callers = append(callers, r)
			continue
		}
		r := e.makeRelated(hit.node, s.h, evidenceGap, "", 0,
			fmt.Sprintf("same-package name match for %s, %s%s", s.h.sel, hit.why, test))
		r.Gap, r.GapFor, r.NameMatch = true, s.h.node.ID, true
		cands = append(cands, r)
	}
	return callers, cands
}

// capSamePkg keeps the best-ranked callers up to the per-declaration cap and counts
// the rest, which the gaps section reports.
func (e *expander) capSamePkg(callers []contextRelated) []contextRelated {
	slices.SortFunc(callers, compareRelated)
	if len(callers) > e.sp.perSeed {
		e.stats.SamePkgMore += len(callers) - e.sp.perSeed
		callers = callers[:e.sp.perSeed]
	}
	return callers
}

// admitSamePkgGaps keeps a declaration's best candidates, up to the per-subject gap
// cap, for the gap section; a declaration found for two subjects keeps its better
// rank.
func (e *expander) admitSamePkgGaps(cands []contextRelated) {
	slices.SortFunc(cands, compareRelated)
	if len(cands) > contextGapPerSubject {
		e.stats.GapMore += len(cands) - contextGapPerSubject
		cands = cands[:contextGapPerSubject]
	}
	for _, r := range cands {
		key := relatedIdentity(r.Node)
		if old, ok := e.sp.gaps[key]; !ok || compareRelated(r, old) < 0 {
			e.sp.gaps[key] = r
		}
	}
}

// adoptedCall is r once the language server has confirmed a call into seed that the
// index could not resolve: a same-package name match is then a resolved caller, and
// no longer only a name match. Anything else is returned as it was.
func (r contextRelated) adoptedCall(seed hopNode, inward bool) contextRelated {
	if inward && r.NameMatch {
		r.NameMatch, r.CallerOf = false, seed.node.ID
		r.Via = viaText(topology.EdgeCalls, true, seed.sel)
	}
	return r
}

// samePkgGapCandidates are the candidates the walk collected that the pool does not
// already hold as a relationship.
func (e *expander) samePkgGapCandidates() []contextRelated {
	if e.sp == nil {
		return nil
	}
	var out []contextRelated
	for _, r := range e.sp.gaps {
		if !e.idents[relatedIdentity(r.Node)] {
			out = append(out, r)
		}
	}
	return out
}
