package tools

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Seed kinds.
const (
	seedFile   = "file"
	seedSymbol = "symbol"
)

// contextSeed is one resolved starting point. It is deliberately a plain
// comparable value: a body, which carries a snapshot, lives beside it in the
// pack (contextBody) and never inside it.
type contextSeed struct {
	Kind     string // seedFile or seedSymbol
	ID       int64  // symbols only: the index node, which expansion walks from
	Path     string // root-relative, slash-separated
	Abs      string // absolute path under the canonical root, for follow-up calls
	Selector string // symbols only: the qualified selector as the index spells it
	Name     string // symbols only: the declaration's bare name
	NodeKind string // symbols only: function, method, type, ...
	Line     int    // symbols only: 1-based declaration line
	EndLine  int    // symbols only: 1-based last line, as the index recorded it
	Language string
	// Coverage is non-empty when the index cannot describe this file, and says
	// why: a coverage gap is never to be read as "no symbols".
	Coverage string
	// Signature and Doc are the index's one-line signature and the first line of
	// its doc comment. They are index text and may be stale; they stand in for a
	// body only when the body cannot be delivered.
	Signature string
	Doc       string
	// Shadowed counts the import, package and file nodes that share the selector
	// and were set aside because a declaration does.
	Shadowed int
	// IndexHash is the content hash the index held for Path when Line and EndLine
	// were read (see indexedHash); "" when it held none. A body is sliced from the
	// index's span only if the snapshot is exactly the bytes this hash describes,
	// never because the index has since caught up with the file.
	IndexHash string
}

// contextCandidate is a labelled possibility. A candidate is never a seed.
type contextCandidate struct {
	Path     string
	Selector string
	NodeKind string
	Line     int
}

// contextMiss is a seed that did not resolve to exactly one thing. Ambiguous
// marks a selector that matched several declarations; Candidates then lists them
// (bounded) and none was chosen.
type contextMiss struct {
	Input      string
	Reason     string
	Ambiguous  bool
	Candidates []contextCandidate
	More       int // candidates beyond contextMaxCandidates
}

// contextPack is the collector's result and the renderer's only input.
type contextPack struct {
	Root        string
	Intent      string
	MaxBytes    int
	ClampedFrom int
	Seeds       []contextSeed
	Misses      []contextMiss
	Gaps        []string
	// Files holds one entry per file whose bodies were attempted: the single
	// snapshot every body from that file was sliced from, and the guard it implies.
	Files []bodyFile
	// Bodies holds one entry per symbol seed, in seed order.
	Bodies []contextBody
	// Related is the bounded neighbourhood of the seeds in rank order, then the
	// gap candidates; Expansion records what produced it and what it could not do.
	Related   []contextRelated
	Expansion contextExpansion
	// LSP is what the language-server refinement of the top symbol seeds did, which
	// is its own freshness component: the zero value says it was not consulted.
	LSP contextLSP
	// Affected is the static test-impact estimate; Constraints are the memories and
	// document sections retrieved as evidence; Have records what the caller's
	// acknowledgements matched.
	Affected    contextAffected
	Constraints []contextConstraint
	Have        haveStats
	// SourceRead is the bytes of source the call read in all (the bodies, then the
	// memories and documents), which never exceeds 4 x max_bytes.
	SourceRead int64
}

// budget is the byte limit for the rendered pack: max_bytes less the reserve the
// connection layer may append to.
func (p *contextPack) budget() int { return p.MaxBytes - contextReserveBytes }

// ContextCollector gathers a context pack for one call. It holds no mutable
// state and, deliberately, no read tracker: collecting a pack never records a
// read. Recording is a separate act, done by the tool and only for a body that
// survived packing and was rendered.
//
// Concurrency: all methods are safe for concurrent use.
type ContextCollector struct {
	storeFor  TopologyForRootFn
	ws        WorkspaceFn
	guard     BoundaryGuard
	contested ContestedFn
	sensitive SensitivePathFn
	testScope func(context.Context) TestScope // optional; without it test packages are named by directory
	deadline  time.Duration                   // zero means contextExpansionDeadline

	// lsp, with lspWarm, refines the top symbol seeds (context_lsp.go); lspDeadline is
	// its sub-deadline, zero meaning contextLSPSubDeadline. Optional: without lsp the
	// pack is structural and says so.
	lsp         ContextLSP
	lspWarm     LSPWarmupFn
	lspDeadline time.Duration

	// beforeBodies is a test seam: it runs after the index has been read and before
	// the first file is snapshotted, which is where a reindex or an edit can land
	// between the two. Nil in production.
	beforeBodies func()
}

// SensitivePathFn reports whether a path's content must be withheld from a
// response. It is the decision write tools already take (WriteDeps.SensitivePathFn,
// the daemon's changeSensitive): path is absolute, from is a copy source or "". nil
// withholds nothing.
type SensitivePathFn = func(ctx context.Context, path, from string) bool

// NewContextCollector returns a collector over the topology indexes storeFor
// supplies, one per canonical root (it may return nil for a root with none, or
// when indexing is disabled; symbol seeds are then reported unresolved with that
// reason and the pack degrades rather than refusing).
func NewContextCollector(storeFor TopologyForRootFn) *ContextCollector {
	return &ContextCollector{storeFor: storeFor}
}

// WithTestScope wires the accessor for the calling agent's test-command shape, so
// an affected package is named by the target run_task accepts. Without it the
// directory is named and no command is guessed, as topology_affected does.
func (c *ContextCollector) WithTestScope(fn func(context.Context) TestScope) *ContextCollector {
	c.testScope = fn
	return c
}

// WithWorkspace wires the per-agent workspace accessor. Without a workspace the
// collector refuses; it never attaches one.
func (c *ContextCollector) WithWorkspace(ws WorkspaceFn) *ContextCollector {
	c.ws = ws
	return c
}

// WithBoundary wires the boundary guard that refuses a path outside what the
// caller may read.
func (c *ContextCollector) WithBoundary(guard BoundaryGuard) *ContextCollector {
	c.guard = guard
	return c
}

// WithContested wires the contested-pin reporter so a relative path is refused
// once the connection's pin is contested.
func (c *ContextCollector) WithContested(fn ContestedFn) *ContextCollector {
	c.contested = fn
	return c
}

// WithSensitive wires the sensitive-path decision, the same one write responses
// and history use, so a body the expansion reaches (not one the caller named) in a
// sensitive file is reduced to its location and a label.
func (c *ContextCollector) WithSensitive(fn SensitivePathFn) *ContextCollector {
	c.sensitive = fn
	return c
}

// expansionDeadline is the wall-clock budget for the walk.
func (c *ContextCollector) expansionDeadline() time.Duration {
	if c.deadline > 0 {
		return c.deadline
	}
	return contextExpansionDeadline
}

// agentRoot resolves the calling agent's canonical root. An unpinned caller is
// refused with the session_start handoff: collecting never pins anything.
func (c *ContextCollector) agentRoot(ctx context.Context) (string, error) {
	ws := ""
	if c.ws != nil {
		ws = c.ws(ctx)
	}
	if ws == "" {
		return "", ClassifyPathRefusal(UnattachedWorkspaceError{Path: "context_for_task"})
	}
	return canonicalRoot(ws), nil
}

// seedInput is one requested seed before resolution.
type seedInput struct {
	kind string
	text string
}

// seedInputs lists the request's seeds, files then symbols, dropping exact
// repeats.
func (r *contextRequest) seedInputs() []seedInput {
	seen := map[seedInput]bool{}
	var out []seedInput
	add := func(kind string, texts []string) {
		for _, t := range texts {
			in := seedInput{kind: kind, text: strings.TrimSpace(t)}
			if !seen[in] {
				seen[in] = true
				out = append(out, in)
			}
		}
	}
	add(seedFile, r.Files)
	add(seedSymbol, r.Symbols)
	return out
}

// Collect resolves req's seeds under the calling agent's root, walks their
// bounded neighbourhood and ranks it, estimates the affected tests, then reads one
// snapshot of each file a body is wanted from and slices the bodies from it, and
// finally retrieves the memories and document sections that bear on the seeds. A
// refusal (no workspace, a boundary violation, a directory seed, a document
// selector) is an error; a seed that simply does not resolve is reported in the
// pack. Everything it reads is keyed by the agent's canonical root, never the
// connection's.
func (c *ContextCollector) Collect(ctx context.Context, req contextRequest) (contextPack, error) {
	root, err := c.agentRoot(ctx)
	if err != nil {
		return contextPack{}, err
	}
	scope, err := newContextScope(root, req.Within, req.Corpora)
	if err != nil {
		return contextPack{}, err
	}
	pack := contextPack{Root: root, Intent: req.Intent, MaxBytes: req.MaxBytes, ClampedFrom: req.clampedFrom}
	index := c.indexFor(root)
	for i, in := range req.seedInputs() {
		if i >= contextMaxSeeds {
			pack.miss(in.text, fmt.Sprintf("over the %d-seed cap; not resolved", contextMaxSeeds))
			continue
		}
		if err := c.collectSeed(ctx, &pack, scope, index, in); err != nil {
			return contextPack{}, err
		}
	}
	// The graph work (the walk and the test-impact estimate) shares one wall-clock
	// budget, so a slow index costs the call that much once, not twice.
	graphCtx, cancel := context.WithTimeout(ctx, c.expansionDeadline())
	defer cancel()
	c.expand(ctx, graphCtx, &pack, scope, index, req)
	c.affectedTests(graphCtx, &pack, scope, index)
	if c.beforeBodies != nil {
		c.beforeBodies()
	}
	// One source-read budget for the whole call: the bodies, the memories and the
	// documents are each read out of it, so what a call reads is bounded once.
	reads := &sourceBudget{limit: int64(contextSourceReadFactor) * int64(req.MaxBytes)}
	bodyGaps := c.collectBodies(ctx, &pack, index.store, reads)
	pack.markStale()
	pack.applyHave(req.Have)
	constraintGaps := c.constraints(ctx, &pack, scope, index, req, reads)
	pack.SourceRead = reads.spent
	pack.Gaps = pack.allGaps(index, bodyGaps, constraintGaps)
	return pack, nil
}

// allGaps assembles the pack's disclosures in the order a reader needs them: what
// the index can and cannot support first, then the bodies, the relationships, the
// test estimate, the constraints and the acknowledgements.
func (p *contextPack) allGaps(index contextIndex, bodyGaps, constraintGaps []string) []string {
	gaps := append(collectGaps(index), bodyGaps...)
	gaps = append(gaps, p.relationGaps()...)
	gaps = append(gaps, p.affectedGaps()...)
	gaps = append(gaps, constraintGaps...)
	gaps = append(gaps, p.haveGaps()...)
	gaps = append(gaps, p.unreadBodyGap()...)
	return append(gaps, coverageGap(index.store, p.Seeds)...)
}

// expand walks the neighbourhood of the resolved seeds, but only when the index
// can be trusted to describe it. A failing, missing or foreign index gets no walk
// at all: a caller or callee read from a snapshot known to be behind is the
// absence claim this tool exists not to make (C09b). graphCtx carries the call's
// graph deadline; the summary that follows the walk is asked of ctx, so a walk
// that ran out of time is still described truthfully.
func (c *ContextCollector) expand(ctx, graphCtx context.Context, pack *contextPack, scope contextScope, index contextIndex, req contextRequest) {
	c.walk(ctx, graphCtx, pack, scope, index, req, true)
}

// walk is the expansion a pack and a hint share. forPack adds what only a pack
// needs, because only a pack slices bodies from the spans the walk reads: the
// content hash the index held with each span.
func (c *ContextCollector) walk(ctx, graphCtx context.Context, pack *contextPack, scope contextScope, index contextIndex, req contextRequest, forPack bool) {
	if len(pack.Seeds) == 0 || !index.usable() {
		return
	}
	seedDirs := map[string]bool{}
	for _, s := range pack.Seeds {
		seedDirs[path.Dir(s.Path)] = true
	}
	ex := newExpander(index.store, scope, newRanker(req.Task, req.Intent, seedDirs), c.sensitiveFor(ctx, pack.Root))
	if forPack {
		ex.indexHash = hashCache(graphCtx, index.store)
	}
	ex.run(graphCtx, ex.addSeeds(graphCtx, pack.Seeds))
	if forPack {
		// Before the pool is ranked, so what the server confirms is ranked as such.
		pack.LSP = c.refineLSP(graphCtx, ex, pack.Root)
	}
	pack.Related = ex.finish(graphCtx)
	pack.Expansion = ex.summarise(ctx, pack.Related)
}

// sensitiveFor adapts the injected decision to the index's root-relative paths.
// It is nil when no decision is wired, which withholds nothing.
func (c *ContextCollector) sensitiveFor(ctx context.Context, root string) func(string) bool {
	if c.sensitive == nil {
		return nil
	}
	return func(rel string) bool { return c.sensitive(ctx, absUnder(root, rel), "") }
}

// markStale marks the relationships that come from a file whose bytes changed
// since the index parsed them. Only files a body was read from can be known to
// have changed; for the rest the index's freshness is the only evidence there is,
// and the pack says so.
func (p *contextPack) markStale() {
	changed := map[string]bool{}
	for _, f := range p.Files {
		if f.Changed {
			changed[f.Path] = true
		}
	}
	if len(changed) == 0 {
		return
	}
	for i := range p.Related {
		r := &p.Related[i]
		r.Stale = changed[r.Node.Path] || changed[r.ViaPath]
	}
}

func (p *contextPack) miss(input, reason string) {
	p.Misses = append(p.Misses, contextMiss{Input: input, Reason: reason})
}

func (c *ContextCollector) collectSeed(ctx context.Context, pack *contextPack, scope contextScope, index contextIndex, in seedInput) error {
	if in.kind == seedFile {
		return c.seedFile(ctx, pack, scope, in.text)
	}
	return c.seedSymbol(ctx, pack, scope, index, in.text)
}

// locate resolves a caller-supplied path the way every path-bearing tool does
// (resolvePath, the contested-pin refusal, the boundary guard) and then places
// it relative to the agent's canonical root. rel is "" when the path is allowed
// to be read but lies outside the root, such as a read-only dependency root,
// and "." for the root itself.
func (c *ContextCollector) locate(ctx context.Context, root, input string) (abs, rel string, err error) {
	resolved, err := resolvePath(ctx, input, c.ws, c.contested)
	if err != nil {
		return "", "", fmt.Errorf("context_for_task: %w", err)
	}
	if err := requireCanonicalPath(resolved); err != nil {
		return "", "", fmt.Errorf("context_for_task: %w", err)
	}
	if err := c.guard.check(ctx, resolved); err != nil {
		return "", "", fmt.Errorf("context_for_task: %w", err)
	}
	canon := canonicalRoot(resolved)
	rel = relWithinRoot(root, resolved)
	if rel == "" && canon == root {
		rel = "." // the root itself: inside, but it names a directory, not a file
	}
	return canon, rel, nil
}

// absUnder joins an index-relative path onto the canonical root.
func absUnder(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

// collectGaps lists what this pack cannot claim about the index it was built
// from. An index that is missing, another root's or failing supports no
// relationship or test-impact claim at all, and the pack says so in the words the
// cases require.
func collectGaps(index contextIndex) []string {
	switch {
	case index.mismatch:
		return []string{"the topology index supplied for this root describes another root and was not consulted: symbol seeds and graph answers are unavailable", labelIndexFailing}
	case index.store == nil:
		return []string{"topology index unavailable: symbol seeds cannot be resolved and no relationship is known", labelIndexFailing}
	}
	if note := indexHealthNote(index.store.Health(), time.Now()); note != "" {
		return []string{note, labelIndexFailing}
	}
	return nil
}
