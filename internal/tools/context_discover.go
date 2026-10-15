package tools

// context_discover.go — seeds from task prose. A request that names no file or
// symbol is answered from its task text: first the code the text names outright
// (a backticked selector, a path to an existing file), then a lexical search of
// the index for declarations whose names carry the task's words. Every seed found
// this way is labelled with how it was found, so the agent can tell a seed it
// chose from one the tool proposed, and the next-best matches are listed beside
// them as candidates, never as seeds.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/plumbkit/plumb/internal/tokenise"
	"github.com/plumbkit/plumb/internal/topology"
)

// Discovery bounds. Three resolved seeds keep a prose-only pack as focused as a
// typical explicit request; the search over-fetches so the scope filter and the
// one-per-declaration rule still leave enough to choose from.
const (
	contextMaxDiscovered    = 3
	contextDiscoverFetch    = 40
	contextDiscoverOthers   = 5
	contextDiscoverMaxTerms = 16
	// contextDiscoverMinScore is one task word in a declaration's name; a seed
	// also needs that name hit itself (discoveredHit.qualifies), so words only a
	// path carries never add up to one.
	contextDiscoverMinScore = 2
)

// Seed origins, as rendered beside each seed.
const (
	originNamed = "named in the task"
	originFound = "found by search"
)

// discoverKinds are the declarations a task can start from.
var discoverKinds = []string{
	string(topology.KindFunction), string(topology.KindMethod),
	string(topology.KindType), string(topology.KindClass),
}

var (
	// taskBacktickRe: `Cart.Total`, `(*Cart).Total`, `internal/x/y.go`.
	taskBacktickRe = regexp.MustCompile("`([^`\\s]{1,200})`")
	// taskPathRe: a bare token with a slash and a file extension.
	taskPathRe = regexp.MustCompile(`(?:^|[\s(\[{"'])((?:\.{0,2}/)?[A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+\.[A-Za-z0-9]{1,8})(?::\d+(?:-\d+)?)?`)
	// taskSelectorShapeRe: what a backticked token must look like to be a selector
	// rather than a command, a flag or a value.
	taskSelectorShapeRe = regexp.MustCompile(`^(?:[A-Za-z0-9_.\-]+/)*[A-Za-z0-9_.\-]+\.[A-Za-z0-9]{1,8}$|^\(?\*?[A-Za-z_][A-Za-z0-9_]*\)?(?:\.[A-Za-z_][A-Za-z0-9_]*)+$`)
	// taskSymbolRe: a selector-shaped token that may be resolved as a symbol.
	taskSymbolRe = regexp.MustCompile(`^[A-Za-z_(*][A-Za-z0-9_.()*\[\]]*$`)
)

// PromptSelectors lifts at most limit explicit selectors out of free text, in order
// of appearance, without duplicates: backticked selectors and paths, then bare
// slash-and-extension paths. A bare word is never a selector: "fix the cart
// total" names nothing. The context-hint hooks and context_for_task's prose
// discovery share it, so both read a prompt the same way.
func PromptSelectors(text string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimRight(s, ".,;:")
		if s == "" || seen[s] || len(out) == limit {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, m := range taskBacktickRe.FindAllStringSubmatch(text, -1) {
		if tok := strings.TrimSuffix(m[1], "()"); taskSelectorShapeRe.MatchString(tok) {
			add(tok)
		}
	}
	for _, m := range taskPathRe.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	return out
}

// contextDiscovery is what prose discovery did, for the renderer: the words it
// searched with, the matches it did not take, and why it found nothing when it
// found nothing.
type contextDiscovery struct {
	Terms  []string
	Others []contextCandidate
	// Note says why the index was not searched (none, the wrong one, or the search
	// failed); "" when it was.
	Note string
	// Excluded counts matches within/corpora left out; Withheld counts matches in
	// sensitive files, which discovery never proposes (nor lists).
	Excluded, Withheld int
	// NotFiles are path-shaped tokens the task named that are no file here.
	NotFiles []string
}

// collectDiscovered resolves seeds for a request that named none, from its task
// prose. The code the task names outright is resolved first; then search fills
// what is left of contextMaxDiscovered resolved seeds, so a named token that
// resolves to nothing never costs the search a slot. A proposal never comes from
// a sensitive file: discovery is the tool's choice, not the caller's, so it is
// held to what expansion is held to and never delivers such a body.
func (c *ContextCollector) collectDiscovered(ctx context.Context, pack *contextPack, scope contextScope, index contextIndex, task string) error {
	d := &contextDiscovery{}
	pack.Discovery = d
	sensitive := c.sensitiveFor(ctx, pack.Root)
	if sensitive == nil {
		sensitive = func(string) bool { return false }
	}
	named, notFiles := namedSeeds(pack.Root, task)
	d.NotFiles = notFiles
	for _, in := range named {
		if len(pack.Seeds) >= contextMaxDiscovered {
			pack.miss(relToRoot(pack.Root, in.text), fmt.Sprintf("over the %d-seed cap for proposals; not resolved "+
				"(name it in files or symbols to include it)", contextMaxDiscovered))
			continue
		}
		if _, err := c.proposeSeed(ctx, pack, scope, index, in, sensitive); err != nil {
			return err
		}
	}
	hits, err := c.searchTask(ctx, d, index, task)
	if err != nil {
		return err
	}
	ranked, excluded := rankDiscovered(hits, d.Terms, scope)
	d.Excluded = excluded
	proposed, failed := map[string]bool{}, 0
	for _, h := range ranked {
		in := seedInput{
			kind: seedSymbol, text: absUnder(pack.Root, h.node.Path) + "#" + nodeSelector(h.node),
			origin: originFound, matched: strings.Join(h.matched, " "),
		}
		switch {
		case sensitive(h.node.Path):
			d.Withheld++
		case pack.seeded(h.node), proposed[in.text]:
			// Already a seed, or the same selector already tried: one
			// ambiguous selector is one miss, not one per declaration.
		case len(pack.Seeds) < contextMaxDiscovered && h.qualifies() && failed < contextMaxDiscovered:
			proposed[in.text] = true
			ok, err := c.proposeSeed(ctx, pack, scope, index, in, sensitive)
			if err != nil {
				return err
			}
			if !ok {
				failed++
			}
		case len(d.Others) < contextDiscoverOthers:
			d.Others = append(d.Others, candidateOf(h.node))
		}
	}
	return nil
}

// relToRoot shows a proposal root-relative: proposals carry absolute paths so
// they resolve on a contested pin, but the pack speaks in relative ones.
func relToRoot(root, text string) string {
	return strings.TrimPrefix(text, root+"/")
}

// searchTask runs the lexical search for the task's words. An index that cannot
// be searched, or a search that fails, is a note, not an error; a cancelled call
// is an error.
func (c *ContextCollector) searchTask(ctx context.Context, d *contextDiscovery, index contextIndex, task string) ([]topology.SearchResult, error) {
	d.Terms = discoverTerms(task)
	if len(d.Terms) == 0 {
		return nil, nil
	}
	if index.store == nil {
		d.Note = "task prose was not searched: " + index.unavailable()
		return nil, nil
	}
	hits, err := index.store.Search(ctx, strings.Join(d.Terms, " "), topology.SearchOpts{Kinds: discoverKinds, Limit: contextDiscoverFetch})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		d.Note = "task prose search failed: " + err.Error()
		return nil, nil
	}
	return hits, nil
}

// proposeSeed resolves one proposal and reports whether it became a seed. A
// proposal that would refuse an explicit seed only becomes a miss, with the
// reason, since the caller did not give it; a cancelled call stays an error. A
// proposal that resolves into a sensitive file is taken back and reported.
func (c *ContextCollector) proposeSeed(ctx context.Context, pack *contextPack, scope contextScope, index contextIndex, in seedInput, sensitive func(string) bool) (bool, error) {
	before, misses := len(pack.Seeds), len(pack.Misses)
	defer func() {
		for i := misses; i < len(pack.Misses); i++ {
			pack.Misses[i].Input = relToRoot(pack.Root, pack.Misses[i].Input)
		}
	}()
	if err := c.collectSeed(ctx, pack, scope, index, in); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		pack.miss(in.text, "proposed from the task prose, but not usable as a seed: "+err.Error())
		return false, nil
	}
	if len(pack.Seeds) == before {
		return false, nil
	}
	if sensitive(pack.Seeds[before].Path) {
		pack.Seeds = pack.Seeds[:before]
		pack.miss(in.text, "a sensitive path: discovery never proposes one; name it in files or symbols to ask for it")
		return false, nil
	}
	pack.Seeds[before].Origin, pack.Seeds[before].Matched = in.origin, in.matched
	return true, nil
}

// seeded reports whether a resolved seed already covers n: a file seed of its
// file, or a symbol seed of the same declaration.
func (p *contextPack) seeded(n topology.Node) bool {
	for _, s := range p.Seeds {
		if s.Path == n.Path && (s.Kind == seedFile || s.Line == n.StartLine) {
			return true
		}
	}
	return false
}

// namedSeeds turns the code the task names outright into seed inputs: a path that
// is an existing regular file under root is a file seed, and a selector-shaped
// token without a slash is a symbol seed. A path-shaped token that is no file
// here is returned apart, so the pack can say so; anything else is dropped.
func namedSeeds(root, task string) (ins []seedInput, notFiles []string) {
	for _, s := range PromptSelectors(task, contextMaxSeeds) {
		switch {
		case isFileUnder(root, s):
			// Absolute, so the proposal also resolves on a contested pin, which
			// refuses relative paths the caller never gave.
			abs := s
			if !filepath.IsAbs(filepath.FromSlash(s)) {
				abs = filepath.Join(root, filepath.FromSlash(s))
			}
			ins = append(ins, seedInput{kind: seedFile, text: abs, origin: originNamed})
		case strings.Contains(s, "/"):
			notFiles = append(notFiles, s)
		case taskSymbolRe.MatchString(s):
			ins = append(ins, seedInput{kind: seedSymbol, text: s, origin: originNamed})
		}
	}
	return ins, notFiles
}

// isFileUnder reports whether s names an existing regular file inside root.
func isFileUnder(root, s string) bool {
	p := filepath.FromSlash(s)
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	rel, err := filepath.Rel(root, filepath.Clean(p))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	info, err := os.Lstat(filepath.Join(root, rel))
	return err == nil && info.Mode().IsRegular()
}

// discoverTerms is the task's searchable words: identifiers whole and split at
// case boundaries, lower-cased, stop words and fragments under three letters
// dropped, first appearance first, at most contextDiscoverMaxTerms. A plural is
// followed by its singular, so the index's "restart" answers "restarts".
func discoverTerms(task string) []string {
	stop := map[string]bool{}
	for _, w := range strings.Fields(stopWords) {
		stop[w] = true
	}
	var out []string
	seen := map[string]bool{}
	add := func(w string) {
		w = strings.ToLower(w)
		if len(w) < 3 || stop[w] || seen[w] || len(out) == contextDiscoverMaxTerms {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	addWord := func(w string) {
		add(w)
		if lw := strings.ToLower(w); len(lw) > 3 && strings.HasSuffix(lw, "s") && !strings.HasSuffix(lw, "ss") && !stop[lw] {
			add(lw[:len(lw)-1])
		}
	}
	for _, word := range strings.FieldsFunc(task, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		addWord(word)
		for _, part := range strings.Fields(tokenise.SplitIdentifier(word)) {
			addWord(part)
		}
	}
	return out
}

// discoveredHit is one search hit with its discovery score.
type discoveredHit struct {
	node     topology.Node
	score    int
	nameHits int
	matched  []string
	fts      float64
}

// qualifies reports whether a hit is strong enough to seed from: at least one
// task word in the declaration's own name, and the minimum score. Words that
// only its path carries never start a pack.
func (h discoveredHit) qualifies() bool {
	return h.nameHits > 0 && h.score >= contextDiscoverMinScore
}

// rankDiscovered scores each in-scope hit by the task words its declaration's
// name carries (two each) and those only its path or qualified name carries (one
// each), words compared by stem and each stem counted once. A declaration in a
// test file loses a point unless the task speaks of tests, so production code
// outranks the tests of it. One hit per declaration is kept, ordered by score,
// then by the search's own rank, then by path and line so equal hits order the
// same way every call.
func rankDiscovered(hits []topology.SearchResult, terms []string, scope contextScope) (out []discoveredHit, excluded int) {
	wantTests := false
	for _, t := range terms {
		wantTests = wantTests || stem(t) == "test"
	}
	seen := map[string]bool{}
	for _, h := range hits {
		n := h.Node
		if !seedableNode(n) || corpusOfPath(n.Path) != corpusCode {
			continue
		}
		if ok, _ := scope.allows(n.Path, corpusCode); !ok {
			excluded++
			continue
		}
		key := fmt.Sprintf("%s#%s@%d", n.Path, nodeSelector(n), n.StartLine)
		if seen[key] {
			continue
		}
		seen[key] = true
		d := scoreDiscovered(h, terms)
		if !wantTests && isTestPath(n.Path) {
			d.score--
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.fts != b.fts {
			return a.fts > b.fts
		}
		if a.node.Path != b.node.Path {
			return a.node.Path < b.node.Path
		}
		return a.node.StartLine < b.node.StartLine
	})
	return out, excluded
}

func scoreDiscovered(h topology.SearchResult, terms []string) discoveredHit {
	n := h.Node
	name := map[string]bool{stem(strings.ToLower(n.Name)): true}
	for _, part := range strings.Fields(tokenise.SplitIdentifier(n.Name)) {
		name[stem(strings.ToLower(part))] = true
	}
	where := strings.ToLower(n.Qualified + " " + n.Path)
	d := discoveredHit{node: n, fts: h.Score}
	counted := map[string]bool{}
	for _, t := range terms {
		st := stem(t)
		if counted[st] {
			continue
		}
		counted[st] = true
		switch {
		case name[st]:
			d.score += 2
			d.nameHits++
			d.matched = append(d.matched, t)
		case strings.Contains(where, st):
			d.score++
			d.matched = append(d.matched, t)
		}
	}
	return d
}
