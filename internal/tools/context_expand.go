package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_expand.go — the bounded neighbourhood of the resolved seeds.
//
// Expansion is one hop at a time through topology.Store.ImpactFrom, so the scope
// filter (root, within, corpora, plumb's own state) runs on EVERY node before it
// is kept or expanded further: a neighbour outside the scope is dropped, and
// nothing is reached through it (Invariant 2). A whole-depth traversal could not
// do that, because it would walk through an excluded node and only the render
// would hide it.
//
// The work is capped (gate v1 work_caps): depth 2, 60 nodes, a 2 s deadline. The
// store returns neighbours in an unspecified order, so every level is sorted by
// the frozen ranking before the cap truncates it; the same index and the same
// request always give the same pack.

// Expansion caps.
const (
	contextMaxExpansionNodes = 60
	contextExpansionDepth    = 2
	contextExpansionDeadline = 2 * time.Second

	// contextHopNodes is the most neighbours one hop fetches for one centre. It is
	// high on purpose: a truncated fetch keeps an arbitrary subset, so the cap that
	// matters is the ranked one above, and this only guards a pathological hub.
	contextHopNodes = 2000

	// contextFileRoots is how many of a file seed's declarations start the walk.
	contextFileRoots = 8
	// contextFileMates is how many same-file declarations one reached file adds.
	contextFileMates = 6
	// contextRelatedBodies is how many of the best-ranked related declarations get a
	// body attempted. The packer still decides how many fit.
	contextRelatedBodies = 8
)

// The evidence ordinal (gate v1). 4, an LSP-confirmed edge, is not produced here:
// LSP is not consulted until A5.
const (
	evidenceGap       = 0 // a name or reverse-import candidate, labelled and never presented as resolved
	evidenceHeuristic = 1 // a heuristic edge (confidence 0.8), such as Python's intra-file calls
	evidenceDerived   = 2 // a derived edge from the call or import resolver (0.9)
	evidenceExtractor = 3 // a syntactic edge the extractor emitted (1.0)
)

// Edge sources the evidence ordinal maps.
const (
	sourceExtractor      = "extractor"
	sourceCallResolver   = "call-resolver"
	sourceImportResolver = "import-resolver"
	sourceHeuristic      = "heuristic"
)

// edgeEvidence maps an edge to the frozen ordinal by its source. An extractor edge
// below full confidence is a guess and is ranked as one; a source this code does
// not know is a candidate, not a fact.
func edgeEvidence(e topology.Edge) int {
	switch e.Source {
	case sourceExtractor:
		if e.Confidence >= 1.0 {
			return evidenceExtractor
		}
		return evidenceHeuristic
	case sourceCallResolver, sourceImportResolver:
		return evidenceDerived
	case sourceHeuristic, "heuristic-ambiguous":
		return evidenceHeuristic
	}
	return evidenceGap
}

// contextRelated is one node the expansion reached, with the relationship that
// reached it.
type contextRelated struct {
	Node     topology.Node // the declaration as the index holds it
	Dist     int           // hops from the nearest root
	Evidence int
	Source   string // edge source, "" for a node reached by structure alone
	Conf     float64
	Via      string // the relationship in words, e.g. "callee of (*Cart).Total"
	ViaPath  string // the file of the node it was reached from, for staleness
	Root     bool   // a declaration of a seed file, not reached through an edge
	Gap      bool   // a reverse-import candidate: evidence 0, never a resolved caller
	Withheld bool   // a sensitive path: location only, never content
	Stale    bool   // its file, or the file it was reached from, changed since indexing
	CallerOf int64  // the node this one is a resolved caller of, when it is
	GapFor   int64  // the seed a gap candidate is evidence about
	// IndexHash is the content hash the index held for the node's file when its span
	// was read ("" when unknown, which no body is sliced on trust of).
	IndexHash string
	Q         float64
	Score     float64
	Body      contextBody // bodyNone until a body is attempted
}

// seedView is the related node as a symbol the body machinery can slice.
func (r contextRelated) seedView(root string) contextSeed {
	return contextSeed{
		Kind: seedSymbol, ID: r.Node.ID, Path: r.Node.Path, Abs: absUnder(root, r.Node.Path),
		Selector: nodeSelector(r.Node), Name: r.Node.Name, NodeKind: string(r.Node.Kind),
		Line: r.Node.StartLine, EndLine: r.Node.EndLine, Language: r.Node.Language,
		Signature: r.Node.Signature, Doc: firstLine(r.Node.Docstring), IndexHash: r.IndexHash,
	}
}

// node rebuilds the index node a symbol seed was resolved from, which is all
// ImpactFrom needs of its centre.
func (s contextSeed) node() topology.Node {
	return topology.Node{
		ID: s.ID, Kind: topology.NodeKind(s.NodeKind), Name: s.Name, Qualified: s.Selector,
		Signature: s.Signature, StartLine: s.Line, EndLine: s.EndLine, Language: s.Language, Path: s.Path,
		Docstring: s.Doc,
	}
}

// expansionStats is what the expansion could not do, for the gaps section.
type expansionStats struct {
	Expanded        int  // nodes whose neighbours were fetched
	Excluded        int  // distinct files the scope filter left out
	Truncated       int  // candidates beyond the node cap
	HopCapped       int  // centres whose neighbour list hit the per-hop ceiling
	MembersCapped   bool // a type has more members than are listed
	MembersListed   bool // a Go type's members were listed, which leave out its fields
	GapMore         int  // gap candidates beyond their quota
	ImportersCapped int  // importer lists that hit the per-hop ceiling, so gap candidates may be missing
	Deadline        bool
	Failed          string // the first store error, if any
}

// hopNode is a node being expanded, and what children should call it.
type hopNode struct {
	node topology.Node
	dist int
	sel  string
}

// expander walks the graph from the roots. It is single-use and single-goroutine.
type expander struct {
	store     *topology.Store
	scope     contextScope
	rk        ranker
	sensitive func(path string) bool
	seedFiles map[string]bool
	// indexHash reports the content hash the index holds for a file (see hashCache);
	// nil where nothing will be sliced from the spans, as for a hint.
	indexHash func(path string) string

	visited  map[int64]bool
	idents   map[string]bool
	excluded map[string]bool
	derived  map[int64]bool
	files    map[string][]topology.Node
	mated    map[string]bool

	seeds []hopNode
	kept  []contextRelated
	stats expansionStats
}

func newExpander(store *topology.Store, scope contextScope, rk ranker, sensitive func(string) bool) *expander {
	return &expander{
		store: store, scope: scope, rk: rk, sensitive: sensitive,
		seedFiles: map[string]bool{}, visited: map[int64]bool{}, idents: map[string]bool{},
		excluded: map[string]bool{}, derived: map[int64]bool{}, files: map[string][]topology.Node{}, mated: map[string]bool{},
	}
}

// relatedIdentity is the key two reaches of one declaration share: its id and its place.
func relatedIdentity(n topology.Node) string {
	return fmt.Sprintf("%d|%s|%s|%s|%d", n.ID, n.Path, n.Kind, nodeSelector(n), n.StartLine)
}

func (e *expander) mark(n topology.Node) {
	e.visited[n.ID] = true
	e.idents[relatedIdentity(n)] = true
}

// fail records why the store could not answer: the deadline, or a store error.
func (e *expander) fail(ctx context.Context, err error) {
	switch {
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		e.stats.Deadline = true
	case e.stats.Failed == "":
		e.stats.Failed = err.Error()
	}
}

// allowed is the scope filter, applied to every node before it is kept or
// expanded. References (imports, package clauses, files) and documents are not
// code relationships and are skipped silently; a node the scope removes is
// counted by file, never described.
func (e *expander) allowed(n topology.Node) bool {
	if topology.IsReference(n.Kind) || n.Kind == topology.KindSection || n.Language == "markdown" {
		return false
	}
	return e.allowedPath(n.Path)
}

func (e *expander) allowedPath(p string) bool {
	ok := plumbStateReason(p) == ""
	if ok {
		ok, _ = e.scope.allows(p, corpusOfPath(p))
	}
	if !ok {
		e.excluded[p] = true
		e.stats.Excluded = len(e.excluded)
	}
	return ok
}

// derivedAdmitted reports whether derived call edges may be used around id: the
// store's admission rule on that node's own subject, asked once per node.
func (e *expander) derivedAdmitted(ctx context.Context, id int64) bool {
	if v, ok := e.derived[id]; ok {
		return v
	}
	v := e.store.DerivedCallsAdmittedFor(ctx, id)
	e.derived[id] = v
	return v
}

// nodesOf returns every indexed node of a file, ordered by line, asked once.
func (e *expander) nodesOf(ctx context.Context, p string) []topology.Node {
	if ns, ok := e.files[p]; ok {
		return ns
	}
	ns, err := e.store.SymbolsInFile(ctx, p)
	if err != nil {
		e.fail(ctx, err)
		return nil
	}
	e.files[p] = ns
	return ns
}

// declsOf is a file's declarations: what nodesOf holds, less references and
// document sections.
func (e *expander) declsOf(ctx context.Context, p string) []topology.Node {
	var out []topology.Node
	for _, n := range e.nodesOf(ctx, p) {
		if !topology.IsReference(n.Kind) && n.Kind != topology.KindSection {
			out = append(out, n)
		}
	}
	return out
}

// makeRelated builds a candidate for n reached from h, scrubs a withheld node of
// everything but its location, and scores it. The scrub comes first, so nothing a
// sensitive file says can reach the ranking, let alone the response.
func (e *expander) makeRelated(n topology.Node, h hopNode, ev int, src string, conf float64, via string) contextRelated {
	r := contextRelated{Node: n, Dist: h.dist + 1, Evidence: ev, Source: src, Conf: conf, Via: via, ViaPath: h.node.Path}
	if e.indexHash != nil {
		r.IndexHash = e.indexHash(n.Path)
	}
	if e.sensitive != nil && e.sensitive(n.Path) {
		r.Withheld = true
		r.Node.Signature, r.Node.Docstring = "", ""
	}
	e.rk.score(&r)
	return r
}

// keep adds r to the pool and, unless it is withheld, returns the node to expand.
func (e *expander) keep(r contextRelated) (hopNode, bool) {
	e.mark(r.Node)
	e.kept = append(e.kept, r)
	if r.Withheld {
		return hopNode{}, false
	}
	return hopNode{node: r.Node, dist: r.Dist, sel: nodeSelector(r.Node)}, true
}

// addSeeds registers the resolved seeds as roots (a symbol seed) or expands a file
// seed into its top declarations. Symbol seeds go first, so a file seed never
// lists a declaration the caller named as a seed.
func (e *expander) addSeeds(ctx context.Context, seeds []contextSeed) []hopNode {
	var roots []hopNode
	for _, s := range seeds {
		e.seedFiles[s.Path] = true
		if s.Kind != seedSymbol {
			continue
		}
		n := s.node()
		e.mark(n)
		h := hopNode{node: n, dist: 0, sel: s.Selector}
		e.seeds = append(e.seeds, h)
		roots = append(roots, h)
	}
	for _, s := range seeds {
		if s.Kind == seedFile {
			roots = append(roots, e.fileRoots(ctx, s)...)
		}
	}
	return roots
}

// fileRoots picks a covered file seed's top declarations as roots, by task
// coverage and then source order. They are related entries at distance 0.
func (e *expander) fileRoots(ctx context.Context, s contextSeed) []hopNode {
	if s.Coverage != "" {
		return nil
	}
	testFile := isTestPath(s.Path)
	var picks []contextRelated
	for _, n := range e.declsOf(ctx, s.Path) {
		if !rootKind(n.Kind, testFile) || e.visited[n.ID] || !e.allowed(n) {
			continue
		}
		r := e.makeRelated(n, hopNode{node: topology.Node{Path: s.Path}, dist: -1}, evidenceExtractor, sourceExtractor, 1.0, "declared in seed file "+s.Path)
		r.Root = true
		picks = append(picks, r)
	}
	slices.SortStableFunc(picks, func(a, b contextRelated) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.Node.StartLine, b.Node.StartLine))
	})
	if len(picks) > contextFileRoots {
		e.stats.Truncated += len(picks) - contextFileRoots
		picks = picks[:contextFileRoots]
	}
	var roots []hopNode
	for _, r := range picks {
		if h, ok := e.keep(r); ok {
			roots = append(roots, h)
		}
	}
	return roots
}

// rootKind is whether a declaration can start a walk: code that does things or is
// a type, and tests when the seed itself is a test file.
func rootKind(k topology.NodeKind, testFile bool) bool {
	switch k {
	case topology.KindFunction, topology.KindMethod, topology.KindType, topology.KindClass:
		return true
	case topology.KindTest:
		return testFile
	}
	return false
}

// run walks outward from roots, one level at a time.
func (e *expander) run(ctx context.Context, roots []hopNode) {
	frontier := roots
	for depth := 0; depth < contextExpansionDepth && len(frontier) > 0; depth++ {
		var cands []contextRelated
		for _, h := range frontier {
			if ctx.Err() != nil {
				e.stats.Deadline = true
				break
			}
			cands = append(cands, e.neighbours(ctx, h)...)
		}
		frontier = e.admitLevel(cands)
	}
}

// admitLevel deduplicates a level's candidates, ranks them, keeps the best that fit
// under the node cap and returns the nodes to expand next.
func (e *expander) admitLevel(cands []contextRelated) []hopNode {
	best := map[string]contextRelated{}
	for _, c := range cands {
		key := relatedIdentity(c.Node)
		if e.idents[key] || e.visited[c.Node.ID] {
			continue
		}
		if old, ok := best[key]; !ok || compareRelated(c, old) < 0 {
			best[key] = c
		}
	}
	fresh := slices.SortedFunc(maps.Values(best), compareRelated)
	if room := max(contextMaxExpansionNodes-len(e.kept), 0); len(fresh) > room {
		e.stats.Truncated += len(fresh) - room
		fresh = fresh[:room]
	}
	var next []hopNode
	for _, r := range fresh {
		if h, ok := e.keep(r); ok {
			next = append(next, h)
		}
	}
	return next
}

// neighbours is everything one hop from h that the scope allows: graph
// neighbours in both directions, a type's members, and the declarations that share
// a reached file.
func (e *expander) neighbours(ctx context.Context, h hopNode) []contextRelated {
	typeCentre := h.node.Kind == topology.KindType || h.node.Kind == topology.KindClass
	res, err := e.store.ImpactFrom(ctx, h.node, contextTraversalOpts(e.derivedAdmitted(ctx, h.node.ID), expansionEdgeKinds(typeCentre)))
	if err != nil {
		e.fail(ctx, err)
		return nil
	}
	e.stats.Expanded++
	out := e.hood(h, res.DependsOn, false)
	out = append(out, e.hood(h, res.DependedOnBy, true)...)
	out = append(out, e.members(ctx, h)...)
	return append(out, e.mates(ctx, h)...)
}

// contextTraversalOpts sizes one hop in one place, so a test can assert the exact
// opts the traversal runs with against topology's own ceilings
// (ClampTraversalOpts): raising a number past a ceiling would silently be
// clamped, which is the defect PLAN-407 removed elsewhere.
func contextTraversalOpts(includeDerived bool, kinds []string) topology.ImpactOpts {
	return topology.ImpactOpts{
		Depth: 1, MaxNodes: contextHopNodes, MaxBytes: topology.MaxTraversalBytes(),
		EdgeKinds: kinds, IncludeDerivedCalls: includeDerived,
	}
}

// expansionEdgeKinds are the relationships a walk follows. A type has no call
// edges of its own: asking for them would invite attributing its methods' calls
// to the type, so they are not followed from one.
func expansionEdgeKinds(typeCentre bool) []string {
	if typeCentre {
		return []string{"contains", "defines", "inherits", "implements"}
	}
	return []string{"calls", "contains", "defines", "inherits", "implements"}
}

// hood turns one direction of a hop's neighbourhood into candidates.
func (e *expander) hood(h hopNode, nb *topology.Neighbourhood, inward bool) []contextRelated {
	if nb == nil {
		return nil
	}
	if nb.Truncated {
		e.stats.HopCapped++
	}
	best := bestEdges(nb.Edges, h.node.ID, inward)
	var out []contextRelated
	for _, n := range nb.Nodes {
		edge, ok := best[n.ID]
		if !ok || n.ID == h.node.ID || !e.allowed(n) {
			continue
		}
		r := e.makeRelated(n, h, edgeEvidence(edge), edge.Source, edge.Confidence, viaText(edge.Kind, inward, h.sel))
		if inward && edge.Kind == topology.EdgeCalls {
			r.CallerOf = h.node.ID
		}
		out = append(out, r)
	}
	return out
}

// bestEdges picks, for each neighbour of centre, the strongest edge to it: the
// highest evidence, then a fixed order, so two edges between the same pair cannot
// make the answer depend on the order the store returned them.
func bestEdges(edges []topology.Edge, centre int64, inward bool) map[int64]topology.Edge {
	best := map[int64]topology.Edge{}
	for _, ed := range edges {
		from, to := ed.FromID, ed.ToID
		if inward {
			from, to = to, from
		}
		if from != centre || to == centre {
			continue
		}
		old, ok := best[to]
		if !ok || cmp.Or(cmp.Compare(edgeEvidence(old), edgeEvidence(ed)), cmp.Compare(ed.Kind, old.Kind), cmp.Compare(ed.ID, old.ID)) < 0 {
			best[to] = ed
		}
	}
	return best
}

// viaText words an edge as the neighbour's relationship to the node it was
// reached from. inward means the neighbour is the edge's source.
func viaText(kind topology.EdgeKind, inward bool, parent string) string {
	words := map[topology.EdgeKind][2]string{
		topology.EdgeCalls:      {"callee of", "caller of"},
		topology.EdgeContains:   {"member of", "container of"},
		topology.EdgeDefines:    {"defined by", "defines"},
		topology.EdgeInherits:   {"base of", "subtype of"},
		topology.EdgeImplements: {"interface of", "implements"},
	}[kind]
	w := words[0]
	if inward {
		w = words[1]
	}
	if w == "" {
		w = "related to (" + string(kind) + ")"
	}
	return w + " " + parent
}
