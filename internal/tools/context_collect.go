package tools

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/topology"
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
	storeFn   func() *topology.Store
	ws        WorkspaceFn
	guard     BoundaryGuard
	contested ContestedFn
	sensitive SensitivePathFn
	deadline  time.Duration // zero means contextExpansionDeadline
}

// SensitivePathFn reports whether a path's content must be withheld from a
// response. It is the decision write tools already take (WriteDeps.SensitivePathFn,
// the daemon's changeSensitive): path is absolute, from is a copy source or "". nil
// withholds nothing.
type SensitivePathFn = func(ctx context.Context, path, from string) bool

// NewContextCollector returns a collector over the topology store storeFn
// supplies (it may return nil when indexing is disabled; symbol seeds are then
// reported unresolved with that reason).
func NewContextCollector(storeFn func() *topology.Store) *ContextCollector {
	return &ContextCollector{storeFn: storeFn}
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

func (c *ContextCollector) store() *topology.Store {
	if c.storeFn == nil {
		return nil
	}
	return c.storeFn()
}

// contextIndex is the topology index as this call may use it. store is nil when
// there is none, or when the one supplied belongs to a different root than the
// calling agent's: foreign is then that other root, kept for the caller's own
// bookkeeping and never shown, since naming another agent's root would disclose
// it. Until the index is keyed by the agent's root, an index of another root is
// refused rather than consulted, because every answer it gave would be about
// someone else's tree.
type contextIndex struct {
	store   *topology.Store
	foreign string
}

// indexFor applies that rule to the connection's store.
func (c *ContextCollector) indexFor(root string) contextIndex {
	store := c.store()
	if store == nil {
		return contextIndex{}
	}
	if sr := canonicalRoot(store.Root()); sr != root {
		return contextIndex{foreign: sr}
	}
	return contextIndex{store: store}
}

// unavailable words why symbols cannot be resolved, or "" when they can.
func (i contextIndex) unavailable(root string) string {
	switch {
	case i.foreign != "":
		return fmt.Sprintf("the topology index belongs to another root, not this agent's (%s), so symbols cannot be "+
			"resolved against it; call session_start with this workspace, or seed by file", root)
	case i.store == nil:
		return "no topology index is available, so symbols cannot be resolved"
	}
	return ""
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
// bounded neighbourhood and ranks it, then reads one snapshot of each file a body
// is wanted from and slices the bodies from it. A refusal (no workspace, a
// boundary violation, a directory seed, a document selector) is an error; a seed
// that simply does not resolve is reported in the pack.
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
	c.expand(ctx, &pack, scope, index, req)
	bodyGaps := c.collectBodies(ctx, &pack, index.store)
	pack.markStale()
	pack.Gaps = append(collectGaps(req, index), bodyGaps...)
	pack.Gaps = append(pack.Gaps, pack.relationGaps()...)
	pack.Gaps = append(pack.Gaps, pack.unreadBodyGap()...)
	pack.Gaps = append(pack.Gaps, coverageGap(index.store, pack.Seeds)...)
	return pack, nil
}

// expand walks the neighbourhood of the resolved seeds, but only when the index
// can be trusted to describe it. A failing, missing or foreign index gets no walk
// at all: a caller or callee read from a snapshot known to be behind is the
// absence claim this tool exists not to make (C09b).
func (c *ContextCollector) expand(ctx context.Context, pack *contextPack, scope contextScope, index contextIndex, req contextRequest) {
	if len(pack.Seeds) == 0 || index.store == nil || index.store.Health().Failing {
		return
	}
	walkCtx, cancel := context.WithTimeout(ctx, c.expansionDeadline())
	defer cancel()
	seedDirs := map[string]bool{}
	for _, s := range pack.Seeds {
		seedDirs[path.Dir(s.Path)] = true
	}
	ex := newExpander(index.store, scope, newRanker(req.Task, req.Intent, seedDirs), c.sensitiveFor(ctx, pack.Root))
	ex.run(walkCtx, ex.addSeeds(walkCtx, pack.Seeds))
	pack.Related = ex.finish(walkCtx)
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
// from, and what a later slice adds. An index that is missing, another root's or
// failing supports no relationship or test-impact claim at all, and the pack
// says so in the words the cases require.
func collectGaps(req contextRequest, index contextIndex) []string {
	var gaps []string
	switch {
	case index.foreign != "":
		gaps = append(gaps, "topology index belongs to another root: symbol seeds and graph answers are refused, and its health is not reported", labelIndexFailing)
	case index.store == nil:
		gaps = append(gaps, "topology index unavailable: symbol seeds cannot be resolved and no relationship is known", labelIndexFailing)
	default:
		if note := indexHealthNote(index.store.Health(), time.Now()); note != "" {
			gaps = append(gaps, note, labelIndexFailing)
		}
	}
	gaps = append(gaps, "affected tests and constraints are not collected yet")
	if len(req.Have) > 0 {
		gaps = append(gaps, "have was accepted but is not applied yet")
	}
	return gaps
}
