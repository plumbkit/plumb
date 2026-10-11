package tools

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_rank_test.go — the frozen ranking (PLAN-462 gate v1): the formula, its
// five terms taken one at a time, the evidence ordinal and the role definition.

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The score is 8·S + 4·Q + 2/(1+d) + 1·E + 0.5·R. Each weight is pinned on its own
// by the difference one unit of its term makes, so changing any one of them, or
// swapping two, fails here rather than quietly reordering every pack.
func TestRankTerms_ScoreIsTheFrozenFormula(t *testing.T) {
	base := rankTerms{Query: 0.25, Dist: 1, Evidence: 2, Role: 1}
	if got, want := base.score(), 4*0.25+2.0/2+2+0.5; !near(got, want) {
		t.Errorf("score = %v, want %v", got, want)
	}
	for _, tc := range []struct {
		name  string
		vary  func(rankTerms) rankTerms
		delta float64
	}{
		{"S: 8 for an explicit seed", func(r rankTerms) rankTerms { r.Seed = true; return r }, 8},
		{"Q: 4 for full coverage", func(r rankTerms) rankTerms { r.Query += 1; return r }, 4},
		{"E: 1 per ordinal step", func(r rankTerms) rankTerms { r.Evidence++; return r }, 1},
		{"R: 0.5 for a production relation", func(r rankTerms) rankTerms { r.Role = 0; return r }, -0.5},
		{"d: 2/(1+d), so a hop from 0 to 1 costs 1", func(r rankTerms) rankTerms { r.Dist = 0; return r }, 2.0 - 2.0/2},
	} {
		if got := tc.vary(base).score() - base.score(); !near(got, tc.delta) {
			t.Errorf("%s: moving the term changed the score by %v, want %v", tc.name, got, tc.delta)
		}
	}
	if got, want := (rankTerms{Seed: true, Query: 0.5, Dist: 1, Evidence: 3, Role: 1}).score(), 8+2+1+3+0.5; !near(got, want) {
		t.Errorf("a full example scored %v, want %v", got, want)
	}
}

// A seed is never ranked among the relations, because it cannot be outranked: the
// lowest an explicit seed can score exceeds the highest a related node can. Bodies
// are read seeds first on that ground.
func TestRank_AnExplicitSeedOutranksEveryRelatedNodeByConstruction(t *testing.T) {
	lowestSeed := rankTerms{Seed: true}.score()
	highestRelated := rankTerms{Query: 1, Evidence: evidenceExtractor, Role: 1}.score()
	if lowestSeed <= highestRelated {
		t.Errorf("a seed can score %v but a related node can score %v", lowestSeed, highestRelated)
	}
}

// The evidence ordinal (gate v1): extractor 3, derived 2, heuristic 1, anything
// else 0. An extractor edge below full confidence is a guess and ranks as one.
func TestEdgeEvidence_MapsSourcesToTheFrozenOrdinal(t *testing.T) {
	for _, tc := range []struct {
		name string
		edge topology.Edge
		want int
	}{
		{"extractor at 1.0", topology.Edge{Source: "extractor", Confidence: 1.0}, 3},
		{"extractor below full confidence", topology.Edge{Source: "extractor", Confidence: 0.8}, 1},
		{"call resolver", topology.Edge{Source: "call-resolver", Confidence: 0.9}, 2},
		{"import resolver", topology.Edge{Source: "import-resolver", Confidence: 0.9}, 2},
		{"heuristic", topology.Edge{Source: "heuristic", Confidence: 0.8}, 1},
		{"ambiguous heuristic", topology.Edge{Source: "heuristic-ambiguous", Confidence: 0.5}, 1},
		{"a source this code does not know", topology.Edge{Source: "lsp", Confidence: 1.0}, 0},
		{"no source", topology.Edge{}, 0},
	} {
		if got := edgeEvidence(tc.edge); got != tc.want {
			t.Errorf("%s: evidence = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestEvidenceNames_AreTheGateVocabulary(t *testing.T) {
	want := map[int]string{3: "extractor", 2: "derived", 1: "heuristic", 0: "gap candidate"}
	for e, name := range want {
		if got := evidenceName(e); got != name {
			t.Errorf("evidenceName(%d) = %q, want %q", e, got, name)
		}
	}
}

// Q is the share of the task's meaningful tokens a node's name, qualified name and
// doc cover: stop words and short fragments do not count, identifiers split at case
// boundaries, and the common inflections fold.
func TestRanker_CoverageIsTheShareOfMeaningfulTaskTokens(t *testing.T) {
	apply := topology.Node{Name: "ApplyCode", Qualified: "(*Cart).ApplyCode", Docstring: "ApplyCode records a discount code that Total will honour."}
	for _, tc := range []struct {
		name string
		task string
		node topology.Node
		want func(float64) bool
		why  string
	}{
		{"no prose", "", apply, func(q float64) bool { return q == 0 }, "no prose scores nothing"},
		{"only stop words", "the and of it is to", apply, func(q float64) bool { return q == 0 }, "stop words carry no meaning"},
		{"unrelated prose", "rotate the tls certificate", apply, func(q float64) bool { return q == 0 }, "no shared token"},
		{"a camel-cased name is split", "apply code", apply, func(q float64) bool { return near(q, 1) }, "ApplyCode is apply + code"},
		{"plural folds to singular", "codes", apply, func(q float64) bool { return near(q, 1) }, "codes meets code"},
		{"a doc word counts", "discount", apply, func(q float64) bool { return near(q, 1) }, "the doc mentions discount"},
		{"half the tokens", "discount rotate", apply, func(q float64) bool { return near(q, 0.5) }, "one of two tokens is covered"},
		{"repeats count once", "code code code rotate", apply, func(q float64) bool { return near(q, 0.5) }, "unique tokens"},
		{"punctuation and case do not matter", "Discount, CODE!", apply, func(q float64) bool { return near(q, 1) }, "words, not bytes"},
	} {
		got := newRanker(tc.task, contextIntentChange, nil).coverage(tc.node)
		if !tc.want(got) {
			t.Errorf("%s: Q = %v (%s)", tc.name, got, tc.why)
		}
	}
}

// Prose never seeds anything by itself: a ranker built from any prose only scores
// the nodes it is handed.
func TestRanker_TaskTokensAreBounded(t *testing.T) {
	words := make([]string, 0, 500)
	for i := range 500 {
		words = append(words, "word"+string(rune('a'+i%26))+string(rune('a'+i/26%26))+"x")
	}
	k := newRanker(strings.Join(words, " "), contextIntentUnderstand, nil)
	if len(k.task) > rankMaxTaskWords {
		t.Errorf("%d task tokens kept, want at most %d", len(k.task), rankMaxTaskWords)
	}
}

// R (gate role_definition): a production symbol in a seed's package or a direct
// relation is 1; a test is 0.5 when the intent is change and 0 when it is
// understand; a document is 0.
func TestRanker_RoleFollowsTheGateDefinition(t *testing.T) {
	seedDirs := map[string]bool{"cart": true}
	prod := func(path string) topology.Node { return topology.Node{Kind: topology.KindFunction, Path: path} }
	test := topology.Node{Kind: topology.KindTest, Path: "pricing/discount_test.go"}
	doc := topology.Node{Kind: topology.KindSection, Path: "docs/pricing.md", Language: "markdown"}
	change := newRanker("", contextIntentChange, seedDirs)
	understand := newRanker("", contextIntentUnderstand, seedDirs)
	for _, tc := range []struct {
		name string
		k    ranker
		n    topology.Node
		dist int
		want float64
	}{
		{"production in a seed's package", change, prod("cart/other.go"), 2, 1},
		{"a direct relation elsewhere", change, prod("pricing/discount.go"), 1, 1},
		{"a distant production symbol elsewhere", change, prod("pricing/discount.go"), 2, 0},
		{"a test under change", change, test, 1, 0.5},
		{"a test under understand", understand, test, 1, 0},
		{"a test file's helper under change", change, topology.Node{Kind: topology.KindFunction, Path: "cart/cart_test.go"}, 0, 0.5},
		{"a document", change, doc, 1, 0},
	} {
		if got := tc.k.role(tc.n, tc.dist); !near(got, tc.want) {
			t.Errorf("%s: R = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Ties break on path, then selector; the order never depends on input order.
func TestCompareRelated_BreaksTiesOnPathThenSelectorAndIsDeterministic(t *testing.T) {
	mk := func(path, qualified string, score float64) contextRelated {
		return contextRelated{Node: topology.Node{Path: path, Name: qualified, Qualified: qualified, Kind: topology.KindFunction, StartLine: 1}, Score: score}
	}
	want := []contextRelated{mk("b/z.go", "Z", 5), mk("a/x.go", "A", 3), mk("a/x.go", "B", 3), mk("a/y.go", "A", 3)}
	for _, perm := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {2, 0, 3, 1}, {1, 3, 0, 2}} {
		var got []contextRelated
		for _, i := range perm {
			got = append(got, want[i])
		}
		slices.SortFunc(got, compareRelated)
		for i := range want {
			if got[i].Node.Path != want[i].Node.Path || nodeSelector(got[i].Node) != nodeSelector(want[i].Node) {
				t.Fatalf("order from %v = %v, want score first, then path, then selector", perm, got)
			}
		}
	}
}

func TestIsTestPath_RecognisesTheIndexedLanguagesConventions(t *testing.T) {
	for p, want := range map[string]bool{
		"cart/cart_test.go": true, "pkg/test_x.py": true, "pkg/x_test.py": true, "web/a.test.ts": true,
		"web/a.spec.js": true, "pkg/__tests__/a.js": true,
		"cart/cart.go": false, "pkg/contest.py": false, "docs/testing.md": false,
	} {
		if got := isTestPath(p); got != want {
			t.Errorf("isTestPath(%q) = %v, want %v", p, got, want)
		}
	}
}
