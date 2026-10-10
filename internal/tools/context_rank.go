package tools

import (
	"cmp"
	"path"
	"strings"
	"unicode"

	"github.com/plumbkit/plumb/internal/tokenise"
	"github.com/plumbkit/plumb/internal/topology"
)

// context_rank.go — the frozen ranking (PLAN-462 gate v1).
//
//	score = 8·S + 4·Q + 2/(1+d) + 1·E + 0.5·R
//
// S is 1 for an explicit seed. Q is the share of the task prose's meaningful
// tokens that a node's name, qualified name and doc cover. d is the hop distance
// from the nearest root, E the evidence ordinal of the relationship that reached
// the node, and R the role. Eligibility, scope, freshness and path constraints are
// hard gates applied before this: they filter, they never score.
//
// Ranking happens before any body is read, and it is deterministic: equal scores
// break on path, then selector (the canonical root is the same for every
// candidate in one call, so it cannot split a tie).

// The frozen weights. One calibration pass on dev ids is allowed; any change to
// these is gate v2.
const (
	rankWeightSeed     = 8.0
	rankWeightQuery    = 4.0
	rankWeightDistance = 2.0
	rankWeightEvidence = 1.0
	rankWeightRole     = 0.5
)

// Role values (gate role_definition): a production symbol in a seed's package or
// a direct relation is 1; a test symbol is 0.5 when the intent is change and 0
// when it is understand; documentation and everything else is 0.
const (
	rankRoleProduction = 1.0
	rankRoleTest       = 0.5
)

// Bounds for the task prose and the node text it is compared with.
const (
	rankMinTokenLen  = 3
	rankMaxTaskWords = 64
	rankMaxDocBytes  = 512
)

// stopWords are words that carry no identifying meaning in task prose.
const stopWords = "a an and are as at be but by can do does for from has have how if in into is it its not of on or so " +
	"than that the then there this to was we what when where which while who why will with without would you your " +
	"fix make change add update need should must also only just all any each some more most other such too very"

// rankTerms are the five inputs to the score, so the formula can be pinned apart
// from any node.
type rankTerms struct {
	Seed     bool
	Query    float64
	Dist     int
	Evidence int
	Role     float64
}

func (t rankTerms) score() float64 {
	s := 0.0
	if t.Seed {
		s = 1
	}
	return rankWeightSeed*s + rankWeightQuery*t.Query + rankWeightDistance/(1+float64(t.Dist)) +
		rankWeightEvidence*float64(t.Evidence) + rankWeightRole*t.Role
}

// ranker scores nodes for one call. Concurrency: immutable after newRanker.
type ranker struct {
	intent   string
	seedDirs map[string]bool // directories of the seeds' files: "a seed's package"
	stop     map[string]bool
	task     map[string]bool // the task prose's meaningful tokens
}

func newRanker(task, intent string, seedDirs map[string]bool) ranker {
	k := ranker{intent: intent, seedDirs: seedDirs, stop: map[string]bool{}, task: map[string]bool{}}
	for _, w := range strings.Fields(stopWords) {
		k.stop[w] = true
	}
	for _, tok := range k.tokens(task) {
		if len(k.task) < rankMaxTaskWords {
			k.task[tok] = true
		}
	}
	return k
}

// tokens splits s into word stems: identifiers are split at case boundaries and
// separators, and stop words and fragments shorter than rankMinTokenLen dropped.
func (k ranker) tokens(s string) []string {
	var out []string
	for _, word := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		for _, part := range strings.Fields(tokenise.SplitIdentifier(word)) {
			if k.stop[part] {
				continue
			}
			if st := stem(part); len(st) >= rankMinTokenLen {
				out = append(out, st)
			}
		}
	}
	return out
}

// stem folds the common English inflections, so "codes" meets "code" and
// "charged" meets "charge". It is deliberately crude: Q only re-ranks, and a
// wrong fold costs a little rank, never a seed.
func stem(w string) string {
	if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		w = w[:len(w)-1]
	}
	switch {
	case len(w) > 5 && strings.HasSuffix(w, "ing"):
		w = w[:len(w)-3]
	case len(w) > 4 && strings.HasSuffix(w, "ed"):
		w = w[:len(w)-2]
	}
	if len(w) > 3 && strings.HasSuffix(w, "e") {
		w = w[:len(w)-1]
	}
	return w
}

// coverage is Q: the share of the task's meaningful tokens that n's name,
// qualified name and doc cover, in [0, 1]. Prose never seeds anything by itself,
// so a node with no other reason to be here scores at most 4·Q.
func (k ranker) coverage(n topology.Node) float64 {
	if len(k.task) == 0 {
		return 0
	}
	doc := n.Docstring
	if len(doc) > rankMaxDocBytes {
		doc = doc[:rankMaxDocBytes]
	}
	have := map[string]bool{}
	for _, tok := range k.tokens(n.Name + " " + n.Qualified + " " + doc) {
		have[tok] = true
	}
	hit := 0
	for tok := range k.task {
		if have[tok] {
			hit++
		}
	}
	return float64(hit) / float64(len(k.task))
}

// role is R for a node d hops from its nearest root.
func (k ranker) role(n topology.Node, dist int) float64 {
	switch {
	case n.Kind == topology.KindSection || n.Language == "markdown":
		return 0
	case isTestNode(n):
		if k.intent == contextIntentChange {
			return rankRoleTest
		}
		return 0
	case dist <= 1 || k.seedDirs[path.Dir(n.Path)]:
		return rankRoleProduction
	}
	return 0
}

// score fills r.Q and r.Score from the node and the relationship that reached it.
func (k ranker) score(r *contextRelated) {
	r.Q = k.coverage(r.Node)
	r.Score = rankTerms{Query: r.Q, Dist: r.Dist, Evidence: r.Evidence, Role: k.role(r.Node, r.Dist)}.score()
}

// compareRelated orders by score, best first, then path, selector, kind and line.
func compareRelated(a, b contextRelated) int {
	return cmp.Or(
		cmp.Compare(b.Score, a.Score),
		cmp.Compare(a.Node.Path, b.Node.Path),
		cmp.Compare(nodeSelector(a.Node), nodeSelector(b.Node)),
		cmp.Compare(a.Node.Kind, b.Node.Kind),
		cmp.Compare(a.Node.StartLine, b.Node.StartLine),
	)
}

// isTestNode reports whether n is a test: a test declaration, or anything in a
// test file.
func isTestNode(n topology.Node) bool {
	return n.Kind == topology.KindTest || isTestPath(n.Path)
}

// isTestPath recognises the test-file conventions of the languages plumb indexes.
func isTestPath(p string) bool {
	base := strings.ToLower(path.Base(p))
	for _, suffix := range []string{"_test.go", "_test.py", "_test.rs", ".test.ts", ".test.tsx", ".test.js", ".spec.ts", ".spec.js", "test.java", "tests.cs"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	if strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") {
		return true
	}
	return strings.Contains("/"+p, "/__tests__/")
}
