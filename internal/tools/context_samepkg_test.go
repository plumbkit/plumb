package tools

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_samepkg_test.go — the callers the call graph cannot resolve, found by name
// in the same package (PLAN-462 gate v2, item 2): the fixture cases, what is
// admitted as a caller at evidence 1 and what only as a gap candidate at evidence 0.

// guardLog records every path the collector's boundary guard was asked about, which
// is every file the collector reads or resolves.
type guardLog struct {
	mu    sync.Mutex
	paths []string
}

func (g *guardLog) add(p string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paths = append(g.paths, p)
}

func (g *guardLog) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paths = nil
}

// saw reports whether any recorded path ends in suffix.
func (g *guardLog) saw(suffix string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.ContainsFunc(g.paths, func(p string) bool { return strings.HasSuffix(p, suffix) })
}

// newSamePkgTool indexes files (Go sources only) and logs the guard's calls.
func newSamePkgTool(t *testing.T, files map[string]string) (shopTool, *guardLog) {
	t.Helper()
	s := newModuleWorkspace(t, files)
	log := &guardLog{}
	inner := testBoundaryGuard(s.root)
	s.collector.WithBoundary(func(ctx context.Context, p string) error {
		log.add(p)
		return inner(ctx, p)
	})
	return s, log
}

// nameMatches lists a pack's same-package name matches by gold id: the callers (e1)
// and the gap candidates (e0), each sorted.
func nameMatches(p contextPack) (callers, cands []string) {
	for _, r := range p.Related {
		switch {
		case r.NameMatch && !r.Gap:
			callers = append(callers, goldID(r.Node.Path, nodeSelector(r.Node)))
		case r.NameMatch:
			cands = append(cands, goldID(r.Node.Path, nodeSelector(r.Node)))
		}
	}
	slices.Sort(callers)
	slices.Sort(cands)
	return callers, cands
}

func requireIDs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// samePkgFunctionFiles is one package of sibling files that mention Apply in every
// way a name can be mentioned.
var samePkgFunctionFiles = map[string]string{
	"p/a.go": `package p

// Apply doubles n; its recursion is no caller of itself.
func Apply(n int) int {
	if n < 0 {
		return Apply(-n)
	}
	return n * 2
}
`,
	"p/b.go": "package p\n\n// Savings calls Apply by its bare name.\nfunc Savings(n int) int { return n - Apply(n) }\n",
	// A local name that shadows the function is not a call of it.
	"p/shadow_param.go": "package p\n\nfunc throughParam(Apply func(int) int) int { return Apply(1) }\n",
	"p/shadow_local.go": "package p\n\nfunc throughLocal() int {\n\tApply := func(n int) int { return n }\n\treturn Apply(2)\n}\n",
	// Another package clause in the same directory is another package: its own Apply
	// is no second declaration of this one, and its call is not a call of this one.
	"p/other_package.go": "package other\n\nfunc Apply() int { return 0 }\n\nfunc Use() int { return Apply() }\n",
	// A call on a receiver is not a bare call of the function, and a method of the
	// same name is no second declaration of it.
	"p/method_call.go": "package p\n\ntype T struct{}\n\nfunc (t T) Apply(n int) int { return n }\n\nfunc (t T) via() int { return t.Apply(4) }\n",
	"p/c_test.go":      "package p\n\nimport \"testing\"\n\nfunc TestApply(t *testing.T) { Apply(5) }\n",
	"p/external_test.go": `package p_test

import (
	"testing"

	"example.com/p"
)

func TestExternal(t *testing.T) { p.Apply(6) }
`,
	// Another directory's Apply, and its caller, are another declaration.
	"q/a.go": "package q\n\nfunc Apply() int { return 0 }\n\nfunc Calls() int { return Apply() }\n",
}

func TestSamePackage_AFunctionIsCalledByNameFromSiblingFiles(t *testing.T) {
	s, log := newSamePkgTool(t, samePkgFunctionFiles)
	args := map[string]any{"symbols": []string{"p/a.go#Apply"}, "intent": contextIntentChange}
	pack := s.collect(t, args)
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}

	callers, cands := nameMatches(pack)
	requireIDs(t, "same-package callers", callers, goldID("p/b.go", "Savings"), goldID("p/c_test.go", "TestApply"))
	requireIDs(t, "gap candidates", cands)
	for _, r := range pack.Related {
		if r.Evidence != evidenceHeuristic || r.CallerOf != 0 || r.Source != "" || r.Dist != 1 || r.Edge != topology.EdgeCalls ||
			!strings.HasPrefix(r.Via, "caller of Apply (same-package name match") {
			t.Errorf("%s: %+v, want a heuristic caller one hop out that is not counted as resolved", r.Node.Name, r)
		}
	}
	for _, want := range []string{
		"[e1 heuristic] Savings — p/b.go:4 function; caller of Apply (same-package name match) — ",
		"TestApply — p/c_test.go:5 test; caller of Apply (same-package name match, test)",
		"callers of Apply: none resolved in the index, plus 2 same-package name match(es) (e1); this is not proof of no callers",
		labelSamePackage,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	requireNeverNoCallers(t, out)

	// Every file the pass read lies under the agent's own root.
	root := canonicalRoot(s.root) + "/"
	for _, p := range log.paths {
		if !strings.HasPrefix(canonicalRoot(p), root) {
			t.Errorf("the collector consulted %s, outside the agent's root %s", p, root)
		}
	}
	if !log.saw("/p/b.go") {
		t.Error("control: the sibling that makes the call was never read")
	}
	if log.saw("/q/a.go") {
		t.Error("another directory's file was read for the package's callers")
	}
}

// A seed is not another seed's caller (it is already a root), and a caller the walk
// already holds is not found twice. Control: the same sibling is listed without the
// second seed.
func TestSamePackage_ASeedIsNotListedAsAnotherSeedsCaller(t *testing.T) {
	s, _ := newSamePkgTool(t, samePkgFunctionFiles)
	both := s.collect(t, map[string]any{"symbols": []string{"p/a.go#Apply", "p/b.go#Savings"}})
	callers, _ := nameMatches(both)
	requireIDs(t, "callers with Savings also a seed", callers, goldID("p/c_test.go", "TestApply"))
	alone := s.collect(t, map[string]any{"symbols": []string{"p/a.go#Apply"}})
	callers, _ = nameMatches(alone)
	requireIDs(t, "control: callers of Apply alone", callers, goldID("p/b.go", "Savings"), goldID("p/c_test.go", "TestApply"))
}

func TestSamePackage_AnAmbiguousFunctionNameIsOnlyAGapCandidate(t *testing.T) {
	variants := map[string]string{
		"p/dup_linux.go": "//go:build linux\n\npackage p\n\nfunc Dup() int { return 1 }\n",
		"p/use.go":       "package p\n\nfunc use() int { return Dup() }\n",
	}
	// Control: one declaration of the name, and the call is a caller.
	single, _ := newSamePkgTool(t, variants)
	callers, cands := nameMatches(single.collect(t, map[string]any{"symbols": []string{"p/dup_linux.go#Dup"}}))
	requireIDs(t, "control: callers of the only Dup", callers, goldID("p/use.go", "use"))
	requireIDs(t, "control: candidates", cands)

	variants["p/dup_other.go"] = "//go:build !linux\n\npackage p\n\nfunc Dup() int { return 2 }\n"
	s, _ := newSamePkgTool(t, variants)
	args := map[string]any{"symbols": []string{"p/dup_linux.go#Dup"}}
	pack := s.collect(t, args)
	callers, cands = nameMatches(pack)
	requireIDs(t, "callers of an ambiguous Dup", callers)
	requireIDs(t, "candidates for an ambiguous Dup", cands, goldID("p/use.go", "use"))
	use, _ := pack.related(goldID("p/use.go", "use"))
	if !use.Gap || use.Evidence != evidenceGap || use.CallerOf != 0 || !strings.Contains(use.Via, "2 declarations of Dup in the package") {
		t.Errorf("use: %+v, want an evidence-0 gap candidate that names the ambiguity", use)
	}
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, gapText(use)) || strings.Contains(out, "[e1 heuristic] use") {
		t.Errorf("the ambiguous match is not rendered as a gap candidate only:\n%s", out)
	}
	requireNeverNoCallers(t, out)
}

var samePkgMethodFiles = map[string]string{
	"p/types.go": `package p

type A struct{}

type B struct{}

type holder struct{ a *A }

func (a *A) Run() int { return 1 }

func (b *B) Run() int { return 2 }

func getA() *A { return &A{} }
`,
	// The callers share a file: the walk adds a reached file's other declarations as
	// file-mates, so everything else sits in a file of its own.
	"p/typed.go": `package p

func (a *A) again() int { return a.Run() }

func viaChainMate() int { return getA().Run() }

func viaParam(a *A) int { return a.Run() }

func viaVar() int {
	var a A
	return a.Run()
}

func viaLiteral() int {
	a := &A{}
	return a.Run()
}

func viaNew() int {
	a := new(A)
	return a.Run()
}
`,
	// One declaration that calls both ways is a caller, whichever call comes first.
	"p/both.go":       "package p\n\nfunc both1(a *A) int { return a.Run() + getA().Run() }\n\nfunc both2(a *A) int { return getA().Run() + a.Run() }\n",
	"p/other_type.go": "package p\n\nfunc viaOther(b *B) int { return b.Run() }\n",
	"p/chain.go":      "package p\n\nfunc viaChain() int { return getA().Run() }\n",
	"p/field.go":      "package p\n\nfunc viaField(h holder) int { return h.a.Run() }\n",
	"p/package.go":    "package p\n\nimport \"strings\"\n\nfunc viaPackage(s string) string { return strings.Run(s) }\n",
	"p/bare.go":       "package p\n\nfunc viaBare(Run func() int) int { return Run() }\n",
}

func TestSamePackage_AMethodNeedsReceiverEvidenceInTheCaller(t *testing.T) {
	s, _ := newSamePkgTool(t, samePkgMethodFiles)
	s.collector.samePkgCap = 50 // this test is about evidence, not the cap
	args := map[string]any{"symbols": []string{"p/types.go#A.Run"}, "intent": contextIntentChange}
	pack := s.collect(t, args)
	callers, cands := nameMatches(pack)
	// A receiver the caller's own declarations show to be an A is evidence: the method's
	// receiver, a parameter, a var, a literal, new(A). A B, a package and a bare
	// function value are other things; a chain and a field select an unknown type.
	requireIDs(t, "callers of A.Run", callers,
		goldID("p/typed.go", "(*A).again"), goldID("p/typed.go", "viaParam"), goldID("p/typed.go", "viaVar"),
		goldID("p/typed.go", "viaLiteral"), goldID("p/typed.go", "viaNew"), goldID("p/both.go", "both1"), goldID("p/both.go", "both2"))
	requireIDs(t, "candidates for A.Run", cands, goldID("p/chain.go", "viaChain"), goldID("p/field.go", "viaField"))
	// An unknown-receiver call in a file the walk reached is already a relationship (a
	// file-mate), so it is not offered a second time as a candidate.
	if mate, ok := pack.related(goldID("p/typed.go", "viaChainMate")); !ok || mate.Gap || mate.NameMatch {
		t.Errorf("viaChainMate: %+v (found=%v), want a file-mate that is neither a candidate nor a name match", mate, ok)
	}
	chain, _ := pack.related(goldID("p/chain.go", "viaChain"))
	if !chain.Gap || chain.Evidence != evidenceGap || !strings.Contains(chain.Via, "the receiver's type is not evident") {
		t.Errorf("viaChain: %+v, want an evidence-0 candidate that says the receiver's type is not evident", chain)
	}

	// The other type's method: its typed caller is its own, and every unknown receiver
	// is a candidate for it too.
	b := s.collect(t, map[string]any{"symbols": []string{"p/types.go#B.Run"}})
	callers, cands = nameMatches(b)
	requireIDs(t, "callers of B.Run", callers, goldID("p/other_type.go", "viaOther"))
	requireIDs(t, "candidates for B.Run", cands, goldID("p/chain.go", "viaChain"), goldID("p/field.go", "viaField"),
		goldID("p/typed.go", "viaChainMate"), goldID("p/both.go", "both1"), goldID("p/both.go", "both2"))

	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	// The count beside the resolved callers is of callers, not of candidates.
	if want := "callers of (*A).Run: none resolved in the index, plus 7 same-package name match(es) (e1)"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	requireNeverNoCallers(t, out)
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "] viaChain — ") && strings.HasPrefix(line, "  [e") {
			t.Errorf("an unknown receiver is presented as a relationship: %q", line)
		}
	}
}

// A method whose declaration appears twice on one receiver (two build variants) is
// ambiguous even with receiver evidence: nothing says which one the call means.
func TestSamePackage_ADuplicatedMethodIsOnlyAGapCandidate(t *testing.T) {
	s, _ := newSamePkgTool(t, map[string]string{
		"p/a_linux.go": "//go:build linux\n\npackage p\n\ntype A struct{}\n\nfunc (a *A) Run() int { return 1 }\n",
		"p/a_other.go": "//go:build !linux\n\npackage p\n\nfunc (a *A) Run() int { return 2 }\n",
		"p/use.go":     "package p\n\nfunc viaParam(a *A) int { return a.Run() }\n",
	})
	callers, cands := nameMatches(s.collect(t, map[string]any{"symbols": []string{"p/a_linux.go#A.Run"}}))
	requireIDs(t, "callers", callers)
	requireIDs(t, "candidates", cands, goldID("p/use.go", "viaParam"))
}

// The shop fixture: Savings calls Apply by its bare name from a sibling file, and the
// tests call Total on a local `var c Cart`.
func TestSamePackage_TheShopFixtureCases(t *testing.T) {
	for _, id := range []string{"C16", "C17"} {
		t.Run(id, func(t *testing.T) {
			s := newShop(t)
			c := oracle(t, id)
			pack, out := runCase(t, s, c)
			checkGold(t, c, pack, out)
			requireNeverNoCallers(t, out)
			for _, rel := range c.gold(t, "related") {
				r, _ := pack.related(rel)
				if !r.NameMatch || r.Evidence != evidenceHeuristic || r.CallerOf != 0 {
					t.Errorf("%s: %+v, want a same-package name match at evidence 1, not a resolved caller", rel, r)
				}
			}
			if !strings.Contains(out, labelSamePackage) {
				t.Errorf("%s: the same-package label is missing:\n%s", id, out)
			}
		})
	}
}

// A caller of a resolved neighbour is found one hop further out, and the walk goes no
// further than its depth cap.
func TestSamePackage_ACallerOfAResolvedNeighbourIsFoundAtTheNextHop(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}, "intent": contextIntentChange})
	savings, ok := pack.related(goldID("pricing/receipt.go", "Savings"))
	if !ok || savings.Dist != 2 || !savings.NameMatch || savings.Via != "caller of Apply (same-package name match)" {
		t.Errorf("Savings: %+v (found=%v), want a name-match caller of the callee Apply, two hops from the seed", savings, ok)
	}
	for _, r := range pack.Related {
		if r.Dist > contextExpansionDepth {
			t.Errorf("%s is %d hops out, past the depth cap of %d", r.Node.Name, r.Dist, contextExpansionDepth)
		}
	}
}

// A hint reads no source, so it searches no sibling file for callers.
func TestSamePackage_AHintFindsNoNameMatches(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"symbols": []string{"pricing/discount.go#Apply"}})
	if callers, _ := nameMatches(pack); !slices.Contains(callers, goldID("pricing/receipt.go", "Savings")) {
		t.Fatalf("control: the pack does not list Savings as a caller: %v", callers)
	}
	res := mustHint(t, shopHinter(s, nil), hintReq(s, ContextSeed{Path: "pricing/discount.go", Symbol: "Apply"}))
	if text := hintText(res); strings.Contains(text, "Savings") || strings.Contains(text, "same-package") {
		t.Errorf("a hint named a same-package match:\n%s", text)
	}
}

// What the language server confirms of a name match is a resolved caller from then
// on; a match for another seed, or a gap candidate, is not confirmed by it.
func TestSamePackage_AServerConfirmedNameMatchBecomesAResolvedCaller(t *testing.T) {
	seed := hopNode{node: topology.Node{ID: 7, Name: "Apply"}, sel: "Apply"}
	r := contextRelated{NameMatch: true, Edge: topology.EdgeCalls, Evidence: evidenceHeuristic, ParentID: 7, Via: "caller of Apply (same-package name match)"}
	if !r.confirmsCall(7, true) || r.confirmsCall(8, true) || r.confirmsCall(7, false) {
		t.Fatalf("confirmsCall(7,in)=%v (8,in)=%v (7,out)=%v, want true false false", r.confirmsCall(7, true), r.confirmsCall(8, true), r.confirmsCall(7, false))
	}
	gap := r
	gap.Gap = true
	if gap.confirmsCall(7, true) {
		t.Error("a gap candidate was confirmed as a call")
	}
	if got := r.adoptedCall(seed, true); got.NameMatch || got.CallerOf != 7 || got.Via != "caller of Apply" {
		t.Errorf("after confirmation: %+v, want a resolved caller of seed 7", got)
	}
	if out := (contextRelated{NameMatch: true}).adoptedCall(seed, false); !out.NameMatch {
		t.Error("an outward confirmation changed a name match")
	}
}
