package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/topology"
)

// context_hint.go — (*ContextCollector).Hint, the collector's side of the
// ContextHinter contract (context_hint_api.go) that the lifecycle hooks consume.
//
// A hint is the cheapest thing the collector can say: where the declarations near
// some seeds live, and why, taken from the index alone.
//
//   - Code corpus only. No memory, no document, no mail.
//   - No bodies and no reads. Hint opens no file and the collector has no read
//     tracker, so nothing it says can be taken for something the agent read.
//   - A selector that is ambiguous, or matches nothing, is a Gap and never a list
//     of candidates: a hook injects this unprompted, so a guess would be read as a
//     fact.
//   - Root, scope and sensitive rules apply at every hop, as they do for a pack. A
//     sensitive path may be named, never its content, and is not walked through.
//   - Bounded by req.MaxBytes and req.Deadline. A deadline that has passed is a
//     partial answer with a Gap, never a late one.
//   - Keyed by req.Workspace, the hooked agent's canonical root, and by nothing else.

var _ ContextHinter = (*ContextCollector)(nil)

const (
	// contextHintDefaultBytes is the per-turn budget of gate v1 (hooks.per_turn_bytes),
	// used when a request names none.
	contextHintDefaultBytes = 1024
	// contextHintLineBytes is what a line costs beyond its text fields.
	contextHintLineBytes = 24
	// contextHintGapBytes and contextHintMaxGaps bound the disclosures, so they
	// leave room for the lines they qualify.
	contextHintGapBytes = 120
	contextHintMaxGaps  = 3
)

// NewContextHinter returns the hint side of the collector for a caller with no
// connection to speak of, the daemon's hook control command. It consults the
// root-keyed index accessor and the sensitive-path decision it is given, and
// nothing else: it has no workspace accessor, boundary guard or read tracker, since
// Hint takes its root from the request and never reads or records. sensitive may be
// nil, which withholds nothing.
func NewContextHinter(storeFor TopologyForRootFn, sensitive SensitivePathFn) ContextHinter {
	return NewContextCollector(storeFor).WithSensitive(sensitive)
}

// Hint answers req from the index of req.Workspace. It returns an error only for a
// fault in the caller; everything the collector could not say is in the result's
// Gaps.
func (c *ContextCollector) Hint(ctx context.Context, req HintRequest) (HintResult, error) {
	hctx, cancel := hintContext(ctx, req.Deadline, c.expansionDeadline())
	defer cancel()
	switch {
	case hctx.Err() != nil:
		return fitHint(nil, []string{hintDeadlineGap}, "unavailable", req.MaxBytes), nil
	case strings.TrimSpace(req.Workspace) == "":
		return fitHint(nil, []string{"no workspace was given, so no index was consulted"}, "unavailable", req.MaxBytes), nil
	}
	root := canonicalRoot(req.Workspace)
	scope, err := newContextScope(root, nil, []string{corpusCode})
	if err != nil {
		return HintResult{}, err
	}
	run := &hintRun{c: c, root: root, scope: scope, index: c.indexFor(root), held: map[string]bool{}}
	run.pack.Root = root
	run.resolveSeeds(hctx, req.Seeds)
	run.expand(hctx)
	return run.result(hctx, req.MaxBytes), nil
}

const hintDeadlineGap = "the deadline passed before the hint was complete; this is a partial answer"

// hintContext bounds a hint by the request's deadline and the collector's own
// graph budget, whichever comes first.
func hintContext(ctx context.Context, deadline time.Time, budget time.Duration) (context.Context, context.CancelFunc) {
	limit := time.Now().Add(budget)
	if !deadline.IsZero() && deadline.Before(limit) {
		limit = deadline
	}
	return context.WithDeadline(ctx, limit)
}

// hintRun is one Hint call's working state.
type hintRun struct {
	c     *ContextCollector
	root  string
	scope contextScope
	index contextIndex
	pack  contextPack     // the resolved seeds and, after expand, the neighbourhood
	gaps  []string        // what the hint could not say
	held  map[string]bool // sensitive seed paths: named, never expanded through
}

func (r *hintRun) gap(g string) {
	for _, have := range r.gaps {
		if have == g {
			return
		}
	}
	r.gaps = append(r.gaps, g)
}

func (r *hintRun) resolveSeeds(ctx context.Context, seeds []ContextSeed) {
	for i, s := range seeds {
		if i >= contextMaxSeeds {
			r.gap(fmt.Sprintf("%d seed(s) beyond the %d-seed cap were not looked up", len(seeds)-i, contextMaxSeeds))
			return
		}
		if ctx.Err() != nil {
			return
		}
		r.resolveSeed(ctx, s)
	}
}

// resolveSeed turns one seed into a resolved seed, or into a gap saying why not.
func (r *hintRun) resolveSeed(ctx context.Context, s ContextSeed) {
	symbol := strings.TrimSpace(s.Symbol)
	if strings.TrimSpace(s.Path) == "" && symbol == "" {
		r.gap("a seed names neither a path nor a symbol")
		return
	}
	rel, abs, why := r.locate(s.Path)
	if why == "" && rel != "" {
		why = r.admit(rel)
	}
	if why != "" {
		r.gap(why)
		return
	}
	if symbol == "" {
		r.fileSeed(ctx, rel, abs)
		return
	}
	r.symbolSeed(ctx, s, rel)
}

// locate places a seed's path under the root: relative to it, or absolute and
// inside it. rel is "" for a bare selector. A path that leaves the root, by ".."
// or by a symlink, is a gap that names only what the caller wrote.
func (r *hintRun) locate(p string) (rel, abs, why string) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", "", ""
	}
	abs = p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(r.root, filepath.FromSlash(p))
	}
	if hasParentTraversal(abs) {
		return "", "", textfmt.TerminalSafeLine(p) + ": an absolute path with '..' is not looked up"
	}
	if rel = relWithinRoot(r.root, abs); rel == "" {
		return "", "", textfmt.TerminalSafeLine(p) + ": outside this agent's workspace root, so it was not looked up"
	}
	return rel, abs, ""
}

// admit applies the scope the hint is always under: code corpus, and never plumb's
// own state.
func (r *hintRun) admit(rel string) string {
	if reason := plumbStateReason(rel); reason != "" {
		return textfmt.TerminalSafeLine(rel) + ": " + reason
	}
	if ok, why := r.scope.allows(rel, corpusOfPath(rel)); !ok {
		return textfmt.TerminalSafeLine(rel) + ": hints cover code only (" + why + ")"
	}
	return ""
}

func (r *hintRun) fileSeed(ctx context.Context, rel, abs string) {
	lang, coverage := fileCoverage(rel)
	if coverage != "" {
		r.gap(textfmt.TerminalSafeLine(rel) + ": " + coverage)
		return
	}
	r.pack.Seeds = append(r.pack.Seeds, contextSeed{Kind: seedFile, Path: rel, Abs: abs, Language: lang})
	r.noteSensitive(ctx, rel)
}

// symbolSeed resolves a selector through the same classification a pack uses. One
// match is a seed; none or several is a gap, with no candidate offered.
func (r *hintRun) symbolSeed(ctx context.Context, s ContextSeed, rel string) {
	input := strings.TrimSpace(s.Symbol)
	if rel != "" {
		input = rel + "#" + input
	}
	if r.index.store == nil {
		r.gap(r.index.unavailable())
		return
	}
	hint := topology.NodeHint{Path: rel}
	nodes, rerr := r.index.store.ResolveNodes(ctx, strings.TrimSpace(s.Symbol), hint)
	m, err := classifySymbolNodes(nodes, rerr, r.scope)
	if err != nil {
		if ctx.Err() == nil {
			r.gap("the index could not resolve a selector: " + textfmt.TerminalSafeLine(err.Error()))
		}
		return
	}
	seeds, misses := len(r.pack.Seeds), len(r.pack.Misses)
	r.pack.addMatch(input, hint.Path, m)
	if len(r.pack.Misses) > misses {
		r.gap(hintMissGap(r.pack.Misses[misses], m, hint.Path))
		return
	}
	if len(r.pack.Seeds) > seeds {
		r.noteSensitive(ctx, r.pack.Seeds[seeds].Path)
	}
}

// hintMissGap words a selector that did not resolve to exactly one declaration. A
// pack lists the candidates under it, labelled; a hint never does, so a miss that
// would have had candidates says only that the selector exists elsewhere.
func hintMissGap(miss contextMiss, m symbolMatch, pathHint string) string {
	reason := miss.Reason
	if m.kind == matchNone && m.elsewhere && len(m.nodes) > 0 {
		reason = fmt.Sprintf("no declaration matches within %s; the selector exists elsewhere in the index", pathHint)
	}
	return fmt.Sprintf("%q: %s", textfmt.TerminalSafeLine(miss.Input), textfmt.TerminalSafeLine(reason))
}

// noteSensitive records a seed whose path is sensitive: it is named, and nothing is
// walked from it.
func (r *hintRun) noteSensitive(ctx context.Context, rel string) {
	if r.c.sensitive != nil && r.c.sensitive(ctx, absUnder(r.root, rel), "") {
		r.held[rel] = true
	}
}

// expand walks outward from the seeds that may be walked, with the pack's own
// expander: scope and the sensitive decision apply to every node before it is kept
// or expanded through, and an index that is missing or failing is not walked at all.
func (r *hintRun) expand(ctx context.Context) {
	var walk []contextSeed
	for _, s := range r.pack.Seeds {
		if !r.held[s.Path] {
			walk = append(walk, s)
		}
	}
	if len(walk) == 0 {
		return
	}
	tmp := contextPack{Root: r.root, Seeds: walk}
	r.c.expand(ctx, ctx, &tmp, r.scope, r.index, contextRequest{Intent: contextIntentUnderstand})
	r.pack.Related, r.pack.Expansion = tmp.Related, tmp.Expansion
}

// result assembles the lines (the seeds, then the neighbourhood in rank order) and
// the gaps, and fits them to the budget.
func (r *hintRun) result(ctx context.Context, maxBytes int) HintResult {
	var gaps []string
	if ctx.Err() != nil || r.pack.Expansion.Stats.Deadline {
		gaps = append(gaps, hintDeadlineGap)
	}
	gaps = append(gaps, r.gaps...)
	switch {
	case r.index.store == nil:
		gaps = append(gaps, collectGaps(r.index)[0])
	case !r.index.usable():
		gaps = append(gaps, labelIndexFailing)
	}
	gaps = append(gaps, r.fileSeedGaps()...)
	if x := r.pack.Expansion; x.Ran && x.Stats.Excluded > 0 {
		gaps = append(gaps, fmt.Sprintf("%d file(s) outside the scope were left out and not walked through", x.Stats.Excluded))
	}
	return fitHint(r.lines(), gaps, hintFreshness(r.index), maxBytes)
}

// fileSeedGaps names a file seed the walk found no declaration of: the index lists
// none for it, which is not the same as the file having none.
func (r *hintRun) fileSeedGaps() []string {
	if !r.index.usable() {
		return nil
	}
	var gaps []string
	for _, s := range r.pack.Seeds {
		if s.Kind != seedFile || r.held[s.Path] {
			continue
		}
		if !slices.ContainsFunc(r.pack.Related, func(rel contextRelated) bool { return rel.Root && rel.Node.Path == s.Path }) {
			gaps = append(gaps, textfmt.TerminalSafeLine(s.Path)+": the index lists no declaration to start from")
		}
	}
	return gaps
}

func (r *hintRun) lines() []HintLine {
	var out []HintLine
	for _, s := range r.pack.Seeds {
		if s.Kind == seedFile && !r.held[s.Path] {
			continue // its declarations arrive below, ranked
		}
		l := HintLine{Selector: s.Selector, Path: s.Path, Kind: s.NodeKind, Provenance: "seed", Line: s.Line}
		if s.Kind == seedFile {
			l.Kind = "file"
		}
		if r.held[s.Path] {
			l.Provenance += "; location only: sensitive path"
		}
		out = append(out, l)
	}
	for _, rel := range r.pack.Related {
		out = append(out, HintLine{
			Selector: nodeSelector(rel.Node), Path: rel.Node.Path, Kind: string(rel.Node.Kind),
			Provenance: hintProvenance(rel), Line: rel.Node.StartLine,
		})
	}
	return out
}

// hintProvenance says how a related declaration was reached and how sure that is.
func hintProvenance(r contextRelated) string {
	s := fmt.Sprintf("e%d %s: %s", r.Evidence, evidenceName(r.Evidence), r.Via)
	if r.Gap {
		s = "e0 gap candidate, not a resolved caller: " + r.Via
	}
	if r.Withheld {
		s += "; location only: sensitive path"
	}
	return s
}

// hintFreshness is the state of the index the lines were taken from.
func hintFreshness(i contextIndex) string {
	if i.store == nil {
		return "unavailable"
	}
	return indexFreshness(i.store.Health())
}

// hintLineBytes is what one line costs against the budget.
func hintLineBytes(l HintLine) int {
	return len(l.Selector) + len(l.Path) + len(l.Kind) + len(l.Provenance) + contextHintLineBytes
}

// fitHint fits the result to budget bytes: the freshness first (when even that
// fits), then the gaps (each clamped, and a few at most, with a count of the rest),
// then the lines in order until one does not fit. Every line and gap is
// terminal-safe. What is dropped is counted, so a short result never reads as a
// complete one.
func fitHint(lines []HintLine, gaps []string, freshness string, budget int) HintResult {
	if budget <= 0 {
		budget = contextHintDefaultBytes
	}
	var res HintResult
	used := 0
	if len(freshness) <= budget {
		res.Freshness, used = freshness, len(freshness)
	}
	dropped := 0
	for i, g := range gaps {
		g = textfmt.ClampBytes(textfmt.TerminalSafeLine(g), contextHintGapBytes)
		if i >= contextHintMaxGaps || used+len(g) > budget {
			dropped++
			continue
		}
		res.Gaps = append(res.Gaps, g)
		used += len(g)
	}
	if note := fmt.Sprintf("%d further gap(s) not listed", dropped); dropped > 0 && used+len(note) <= budget {
		res.Gaps = append(res.Gaps, note)
		used += len(note)
	}
	for i, l := range lines {
		l.Selector, l.Path = textfmt.TerminalSafeLine(l.Selector), textfmt.TerminalSafeLine(l.Path)
		l.Kind, l.Provenance = textfmt.TerminalSafeLine(l.Kind), textfmt.TerminalSafeLine(l.Provenance)
		n := hintLineBytes(l)
		if used+n > budget {
			res.Omitted = len(lines) - i
			break
		}
		res.Lines = append(res.Lines, l)
		used += n
	}
	return res
}
