package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/topology"
)

// Seed kinds.
const (
	seedFile   = "file"
	seedSymbol = "symbol"
)

// contextSeed is one resolved starting point.
type contextSeed struct {
	Kind     string // seedFile or seedSymbol
	Path     string // root-relative, slash-separated
	Abs      string // absolute path under the canonical root, for follow-up calls
	Selector string // symbols only: the qualified selector as the index spells it
	NodeKind string // symbols only: function, method, type, ...
	Line     int    // symbols only: 1-based declaration line
	Language string
	// Coverage is non-empty when the index cannot describe this file, and says
	// why: a coverage gap is never to be read as "no symbols".
	Coverage string
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
}

// budget is the byte limit for the rendered pack: max_bytes less the reserve the
// connection layer may append to.
func (p *contextPack) budget() int { return p.MaxBytes - contextReserveBytes }

// ContextCollector gathers a context pack for one call. It holds no mutable
// state and, deliberately, no read tracker: collecting a pack never records a
// read. Recording is a separate act, done only for a body that is actually
// delivered.
//
// Concurrency: all methods are safe for concurrent use.
type ContextCollector struct {
	storeFn   func() *topology.Store
	ws        WorkspaceFn
	guard     BoundaryGuard
	contested ContestedFn
}

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

func (c *ContextCollector) store() *topology.Store {
	if c.storeFn == nil {
		return nil
	}
	return c.storeFn()
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

// Collect resolves req's seeds under the calling agent's root. A refusal (no
// workspace, a boundary violation, a directory seed, a document selector) is an
// error; a seed that simply does not resolve is reported in the pack.
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
	store := c.store()
	for i, in := range req.seedInputs() {
		if i >= contextMaxSeeds {
			pack.miss(in.text, fmt.Sprintf("over the %d-seed cap; not resolved", contextMaxSeeds))
			continue
		}
		if err := c.collectSeed(ctx, &pack, scope, store, in); err != nil {
			return contextPack{}, err
		}
	}
	pack.Gaps = collectGaps(req, store)
	return pack, nil
}

func (p *contextPack) miss(input, reason string) {
	p.Misses = append(p.Misses, contextMiss{Input: input, Reason: reason})
}

func (c *ContextCollector) collectSeed(ctx context.Context, pack *contextPack, scope contextScope, store *topology.Store, in seedInput) error {
	if in.kind == seedFile {
		return c.seedFile(ctx, pack, scope, in.text)
	}
	return c.seedSymbol(ctx, pack, scope, store, in.text)
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

// seedFile resolves one file seed: it must exist, be a regular file inside the
// root and pass the scope filter. A directory is refused outright. No body is
// read here.
func (c *ContextCollector) seedFile(ctx context.Context, pack *contextPack, scope contextScope, input string) error {
	abs, rel, err := c.locate(ctx, pack.Root, input)
	if err != nil {
		return err
	}
	if rel == "" {
		pack.miss(input, "outside the workspace root")
		return nil
	}
	info, statErr := os.Stat(abs)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		pack.miss(input, "file not found")
		return nil
	case statErr != nil:
		pack.miss(input, "cannot be read")
		return nil
	case info.IsDir():
		return badArgument(fmt.Errorf("context_for_task: %q is a directory, and a directory is scope, not a seed. "+
			"Narrow with within, or seed with a file inside it (find_files or workspace_search will name candidates)", input))
	case !info.Mode().IsRegular():
		pack.miss(input, "not a regular file")
		return nil
	}
	if ok, why := scope.allows(rel, corpusOfPath(rel)); !ok {
		pack.miss(input, why)
		return nil
	}
	lang, coverage := fileCoverage(rel)
	pack.Seeds = append(pack.Seeds, contextSeed{Kind: seedFile, Path: rel, Abs: abs, Language: lang, Coverage: coverage})
	return nil
}

// fileCoverage names the file's language and, when the index has no extractor
// for it, says so. An uncovered file has unknown symbols, which is not the same
// as having none.
func fileCoverage(rel string) (language, coverage string) {
	l, ok := langsupport.ByPath(rel)
	switch {
	case !ok:
		return "", "unrecognised file type: no extractor, so symbols and relations are unknown, not absent"
	case l.Structural == langsupport.EngineNone:
		return l.Name, l.Name + " is not covered by an extractor: symbols and relations are unknown, not absent"
	}
	return l.Name, ""
}

// splitSymbolSeed splits "path#Selector" at the first '#'; a bare selector has
// no path. Symbols are code selectors only, so a path that names a document is
// refused with a handoff to corpora.
func splitSymbolSeed(input string) (pathPart, selector string, err error) {
	pathPart, selector, found := strings.Cut(input, "#")
	if !found {
		return "", strings.TrimSpace(input), nil
	}
	pathPart, selector = strings.TrimSpace(pathPart), strings.TrimSpace(selector)
	if selector == "" {
		return "", "", badArgument(fmt.Errorf("context_for_task: symbol %q has no selector after '#'", input))
	}
	if pathPart != "" && isDocsPath(pathPart) {
		return "", "", badArgument(fmt.Errorf("context_for_task: symbol %q names a document, but symbols are code selectors. "+
			"Documents come through corpora: pass a code seed with corpora including \"docs\", or read the section with read_file", input))
	}
	return pathPart, selector, nil
}

// seedSymbol resolves one symbol seed through the topology index. Zero or
// several matches are reported, never resolved by choosing one.
func (c *ContextCollector) seedSymbol(ctx context.Context, pack *contextPack, scope contextScope, store *topology.Store, input string) error {
	pathPart, selector, err := splitSymbolSeed(input)
	if err != nil {
		return err
	}
	var hint topology.NodeHint
	if pathPart != "" {
		_, rel, lerr := c.locate(ctx, pack.Root, pathPart)
		if lerr != nil {
			return lerr
		}
		if rel == "" {
			pack.miss(input, "outside the workspace root")
			return nil
		}
		if rel != "." {
			hint.PathSubstr = rel
		}
	}
	if store == nil {
		pack.miss(input, "no topology index is available, so symbols cannot be resolved")
		return nil
	}
	nodes, rerr := store.ResolveNodes(ctx, selector, hint)
	match, err := classifySymbolNodes(nodes, rerr, scope)
	if err != nil {
		return withIndexHealthErr(store, fmt.Errorf("context_for_task: resolving %q: %w", input, err))
	}
	pack.addMatch(input, hint.PathSubstr, match)
	return nil
}

type symbolMatchKind int

const (
	matchNone symbolMatchKind = iota
	matchOne
	matchAmbiguous
)

// symbolMatch is the outcome of resolving one selector. It is kept private and
// small on purpose: the shared resolution result type is being built elsewhere,
// and this is the only seam that needs to change when it lands.
type symbolMatch struct {
	kind  symbolMatchKind
	nodes []topology.Node // the candidates that survived the scope filter
	// elsewhere marks nodes that matched the selector but not the path hint:
	// they are labelled candidates for a "no match here" answer, never seeds.
	elsewhere bool
	// excluded counts candidates the scope filter removed. Only the count is
	// kept, so nothing about an excluded path can reach the pack.
	excluded int
}

// classifySymbolNodes sorts a ResolveNodes outcome into none, one or ambiguous.
// Imports, package clauses and document sections are not declarations a
// selector can seed, and candidates outside the scope are dropped (and counted)
// before classifying, so ambiguity is judged among what the agent may see.
func classifySymbolNodes(nodes []topology.Node, err error, scope contextScope) (symbolMatch, error) {
	var m symbolMatch
	if err != nil {
		mismatch, ok := errors.AsType[*topology.HintMismatchError](err)
		if !ok {
			return symbolMatch{}, err
		}
		nodes, m.elsewhere = mismatch.Candidates, true
	}
	for _, n := range nodes {
		if !seedableNode(n) {
			continue
		}
		if ok, _ := scope.allows(n.Path, corpusCode); !ok {
			m.excluded++
			continue
		}
		m.nodes = append(m.nodes, n)
	}
	switch {
	case m.elsewhere || len(m.nodes) == 0:
		m.kind = matchNone
	case len(m.nodes) == 1:
		m.kind = matchOne
	default:
		m.kind = matchAmbiguous
	}
	return m, nil
}

// seedableNode reports whether n is a declaration a symbol selector may name.
func seedableNode(n topology.Node) bool {
	switch n.Kind {
	case topology.KindImport, topology.KindPackage, topology.KindFile, topology.KindSection:
		return false
	}
	return n.Language != "markdown"
}

// nodeSelector is the selector text to paste back into symbols: the qualified
// name when the index has one, else the bare name.
func nodeSelector(n topology.Node) string {
	if n.Qualified != "" {
		return n.Qualified
	}
	return n.Name
}

func candidateOf(n topology.Node) contextCandidate {
	return contextCandidate{Path: n.Path, Selector: nodeSelector(n), NodeKind: string(n.Kind), Line: n.StartLine}
}

// addMatch records a classified outcome on the pack.
func (p *contextPack) addMatch(input, pathHint string, m symbolMatch) {
	switch m.kind {
	case matchOne:
		n := m.nodes[0]
		p.Seeds = append(p.Seeds, contextSeed{
			Kind: seedSymbol, Path: n.Path, Abs: absUnder(p.Root, n.Path), Selector: nodeSelector(n),
			NodeKind: string(n.Kind), Line: n.StartLine, Language: n.Language,
		})
	case matchAmbiguous:
		miss := contextMiss{
			Input:     input,
			Ambiguous: true,
			Reason:    fmt.Sprintf("%d declarations match; none was chosen", len(m.nodes)),
		}
		miss.Candidates, miss.More = boundedCandidates(m.nodes)
		p.Misses = append(p.Misses, miss)
	default:
		miss := contextMiss{Input: input, Reason: noMatchReason(pathHint, m)}
		miss.Candidates, miss.More = boundedCandidates(m.nodes)
		p.Misses = append(p.Misses, miss)
	}
}

// noMatchReason words a none outcome so it cannot be read as a suggestion: the
// candidates under it, if any, are labelled and are not seeds.
func noMatchReason(pathHint string, m symbolMatch) string {
	reason := "no indexed declaration matches; nothing was invented"
	if m.elsewhere && len(m.nodes) > 0 {
		reason = fmt.Sprintf("no declaration matches within %s; the selector exists elsewhere (candidates, not seeds)", pathHint)
	}
	if m.excluded > 0 {
		reason += fmt.Sprintf("; %d more excluded by within/corpora", m.excluded)
	}
	return reason
}

func boundedCandidates(nodes []topology.Node) (shown []contextCandidate, more int) {
	for i, n := range nodes {
		if i >= contextMaxCandidates {
			return shown, len(nodes) - contextMaxCandidates
		}
		shown = append(shown, candidateOf(n))
	}
	return shown, 0
}

// absUnder joins an index-relative path onto the canonical root.
func absUnder(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

// collectGaps lists what this pack cannot claim. A1 collects seeds only, and
// says so; later chunks narrow the list as they add bodies and relationships.
func collectGaps(req contextRequest, store *topology.Store) []string {
	var gaps []string
	if store == nil {
		gaps = append(gaps, "topology index unavailable: symbol seeds cannot be resolved and no relationship is known")
	} else if note := indexHealthNote(store.Health(), time.Now()); note != "" {
		gaps = append(gaps, note)
	}
	gaps = append(gaps, "bodies, neighbours, callers, affected tests and constraints are not collected yet")
	if req.Task != "" || len(req.Have) > 0 {
		gaps = append(gaps, "task and have were accepted but are not applied yet")
	}
	return gaps
}
