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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/plumbkit/plumb/internal/tokenise"
	"github.com/plumbkit/plumb/internal/topology"
)

// Discovery bounds. Three seeds keep a prose-only pack as focused as a typical
// explicit request; the search over-fetches so the scope filter and the
// one-per-declaration rule still leave enough to choose from.
const (
	contextMaxDiscovered    = 3
	contextDiscoverFetch    = 40
	contextDiscoverOthers   = 5
	contextDiscoverMaxTerms = 16
	// contextDiscoverMinScore is one task word in a declaration's name: a match
	// in a docstring or a path alone is too weak to start a pack from.
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
// searched with and the matches it did not take.
type contextDiscovery struct {
	Terms  []string
	Others []contextCandidate
	// Note says why the index was not searched (none, the wrong one, or the search
	// failed); "" when it was, or when the task's own names left no room.
	Note string
}

// discoverSeeds proposes seeds for a request that gave none. Named seeds come
// first and take precedence; search fills what is left of contextMaxDiscovered.
func (c *ContextCollector) discoverSeeds(ctx context.Context, pack *contextPack, scope contextScope, index contextIndex, task string) []seedInput {
	d := &contextDiscovery{}
	pack.Discovery = d
	ins := namedSeeds(pack.Root, task)
	if len(ins) >= contextMaxDiscovered {
		return ins[:contextMaxDiscovered]
	}
	d.Terms = discoverTerms(task)
	if len(d.Terms) == 0 {
		return ins
	}
	if index.store == nil {
		d.Note = "task prose was not searched: " + index.unavailable()
		return ins
	}
	hits, err := index.store.Search(ctx, strings.Join(d.Terms, " "), topology.SearchOpts{Kinds: discoverKinds, Limit: contextDiscoverFetch})
	if err != nil {
		d.Note = "task prose search failed: " + err.Error()
		return ins
	}
	for _, h := range rankDiscovered(hits, d.Terms, scope) {
		if len(ins) < contextMaxDiscovered && h.score >= contextDiscoverMinScore && !namesSeedFile(ins, h.node.Path) {
			ins = append(ins, seedInput{kind: seedSymbol, text: h.node.Path + "#" + nodeSelector(h.node), origin: originFound, matched: strings.Join(h.matched, " ")})
			continue
		}
		if len(d.Others) < contextDiscoverOthers {
			d.Others = append(d.Others, candidateOf(h.node))
		}
	}
	return ins
}

// namesSeedFile reports whether a named file seed already covers path, so search
// does not add a declaration from a file the task itself named.
func namesSeedFile(ins []seedInput, path string) bool {
	for _, in := range ins {
		if in.kind == seedFile && filepath.ToSlash(in.text) == path {
			return true
		}
	}
	return false
}

// namedSeeds turns the code the task names outright into seed inputs: a path that
// is an existing regular file under root is a file seed, a selector-shaped token
// without a slash is a symbol seed, and anything else is dropped, not guessed at.
func namedSeeds(root, task string) []seedInput {
	var out []seedInput
	for _, s := range PromptSelectors(task, contextMaxSeeds) {
		switch {
		case isFileUnder(root, s):
			out = append(out, seedInput{kind: seedFile, text: s, origin: originNamed})
		case taskSymbolRe.MatchString(s) && !strings.Contains(s, "/"):
			out = append(out, seedInput{kind: seedSymbol, text: s, origin: originNamed})
		}
	}
	return out
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
	node    topology.Node
	score   int
	matched []string
	fts     float64
}

// rankDiscovered scores each in-scope hit by the task words its declaration's
// name carries (two each) and those only its path or qualified name carries (one
// each), words compared by stem and each stem counted once. A declaration in a
// test file loses a point unless the task speaks of tests, so production code
// outranks the tests of it. One hit per declaration is kept, ordered by score,
// then by the search's own rank, then by path and line so equal hits order the
// same way every call.
func rankDiscovered(hits []topology.SearchResult, terms []string, scope contextScope) []discoveredHit {
	wantTests := false
	for _, t := range terms {
		wantTests = wantTests || stem(t) == "test"
	}
	var out []discoveredHit
	seen := map[string]bool{}
	for _, h := range hits {
		n := h.Node
		if !seedableNode(n) || corpusOfPath(n.Path) != corpusCode {
			continue
		}
		if ok, _ := scope.allows(n.Path, corpusCode); !ok {
			continue
		}
		key := n.Path + "#" + nodeSelector(n)
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
	return out
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
			d.matched = append(d.matched, t)
		case strings.Contains(where, st):
			d.score++
			d.matched = append(d.matched, t)
		}
	}
	return d
}
