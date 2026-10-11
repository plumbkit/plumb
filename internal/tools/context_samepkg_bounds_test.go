package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_samepkg_bounds_test.go — what bounds the same-package search: the scope
// filter before a file is read (Invariant 2), sensitive files named and never read,
// the per-declaration cap, and a sibling that changed since it was indexed.

// calcFile declares the method the callers in these tests call on a local Calc: a
// method, because the index resolves a function's bare callers itself, on a local,
// because an index from before the local tier (newSamePkgTool) cannot resolve a
// receiver that is only a local variable.
const calcFile = "package p\n\ntype Calc struct{}\n\nfunc (c *Calc) Apply(n int) int { return n }\n"

// The scope filter runs on the sibling before it is read: a file outside `within`
// is neither listed nor opened, and the pack counts it as excluded. Control: the
// same call without `within` reads it and lists its caller.
func TestSamePackage_TheScopeFilterRunsBeforeASiblingIsRead(t *testing.T) {
	s, log := newSamePkgTool(t, samePkgSiblingFiles)
	seed := []string{"p/a.go#Calc.Apply"}

	open := s.collect(t, map[string]any{"symbols": seed})
	if callers, _ := nameMatches(open); len(callers) != 2 || !log.saw("/p/b.go") {
		t.Fatalf("control: the unscoped pack lists %v and read b.go=%v", callers, log.saw("/p/b.go"))
	}

	log.reset()
	narrow := s.collect(t, map[string]any{"symbols": seed, "within": []string{"p/a.go", "p/c_test.go"}})
	callers, _ := nameMatches(narrow)
	requireIDs(t, "callers within p/a.go and p/c_test.go", callers, goldID("p/c_test.go", "TestApply"))
	if log.saw("/p/b.go") {
		t.Error("a sibling outside within was read")
	}
	if !log.saw("/p/c_test.go") {
		t.Error("control: the sibling inside within was never read")
	}
	if narrow.Expansion.Stats.Excluded == 0 {
		t.Error("the files the scope left out are not counted")
	}
}

// The filter runs at the second hop too: the callee Apply (reached through the typed
// parameter d) is inside `within`, but its callers in the next file over are not, so
// they are left out, while the callers in the seed's own directory are kept. Control:
// without within the sibling is found.
func TestSamePackage_TheScopeFilterRunsAtTheSecondHop(t *testing.T) {
	s, log := newSamePkgTool(t, map[string]string{
		"go.mod":              chainModule,
		"cart/cart.go":        "package cart\n\nimport \"example.com/x/pricing\"\n\nfunc Total(d *pricing.Discount) int { return d.Apply(1) }\n",
		"pricing/discount.go": "package pricing\n\ntype Discount struct{}\n\nfunc (d *Discount) Apply(n int) int { return n }\n",
		"pricing/receipt.go":  "package pricing\n\nfunc Savings(n int) int {\n\tvar d Discount\n\treturn n - d.Apply(n)\n}\n",
	})
	seed := []string{"cart/cart.go#Total"}
	callers, _ := nameMatches(s.collect(t, map[string]any{"symbols": seed}))
	requireIDs(t, "control: callers of the callee Apply", callers, goldID("pricing/receipt.go", "Savings"))

	log.reset()
	pack := s.collect(t, map[string]any{"symbols": seed, "within": []string{"cart", "pricing/discount.go"}})
	if _, ok := pack.related(goldID("pricing/discount.go", "(*Discount).Apply")); !ok {
		t.Fatalf("control: the callee inside within is missing: %v", relatedIDs(pack))
	}
	callers, _ = nameMatches(pack)
	requireIDs(t, "callers of Apply with receipt.go outside within", callers)
	if log.saw("/pricing/receipt.go") {
		t.Error("a sibling outside within was read at the second hop")
	}
}

// A sensitive sibling is named at evidence 0 as a location, never read, and never a
// caller; nothing it says reaches the response. Control: not sensitive, it is read
// and listed as a caller with its signature.
func TestSamePackage_ASensitiveSiblingIsNamedAndNeverRead(t *testing.T) {
	files := map[string]string{
		"p/a.go":      calcFile,
		"p/b.go":      "package p\n\nfunc Savings(n int) int {\n\tvar c Calc\n\treturn n - c.Apply(n)\n}\n",
		"p/secret.go": "package p\n\n// LEAK-MARKER\nfunc Leak(n int) int {\n\tvar c Calc\n\treturn c.Apply(n) + 1\n}\n",
	}
	args := map[string]any{"symbols": []string{"p/a.go#Calc.Apply"}}

	open, openLog := newSamePkgTool(t, files)
	pack := open.collect(t, args)
	leak, ok := pack.related(goldID("p/secret.go", "Leak"))
	if !ok || leak.Gap || leak.Withheld || leak.Evidence != evidenceHeuristic || !openLog.saw("/p/secret.go") {
		t.Fatalf("control: Leak = %+v (found=%v), read=%v; want an ordinary e1 caller whose file was read", leak, ok, openLog.saw("/p/secret.go"))
	}

	s, log := newSamePkgTool(t, files)
	s.collector.WithSensitive(func(_ context.Context, p string, _ string) bool {
		return strings.HasSuffix(filepath.ToSlash(p), "/p/secret.go")
	})
	pack = s.collect(t, args)
	leak, ok = pack.related(goldID("p/secret.go", "Leak"))
	if !ok || !leak.Gap || !leak.Withheld || leak.Evidence != evidenceGap || leak.CallerOf != 0 || leak.Node.Signature != "" {
		t.Fatalf("Leak = %+v (found=%v), want a withheld evidence-0 candidate with no signature", leak, ok)
	}
	if log.saw("/p/secret.go") {
		t.Error("a sensitive file was read")
	}
	callers, cands := nameMatches(pack)
	requireIDs(t, "callers", callers, goldID("p/b.go", "Savings"))
	requireIDs(t, "candidates", cands, goldID("p/secret.go", "Leak"))
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "LEAK-MARKER") || strings.Contains(out, "func Leak") || !strings.Contains(out, gapText(leak)) {
		t.Errorf("a sensitive sibling is not named as a location only:\n%s", out)
	}
}

// The cap is per declaration: the best-ranked callers are listed (production code
// before tests), the rest are counted and the pack says how many. Control: a
// declaration under the cap loses none and says nothing.
func TestSamePackage_ThePerDeclarationCapKeepsTheBestAndSaysSo(t *testing.T) {
	files := map[string]string{
		"p/a.go":      calcFile,
		"p/x_test.go": "package p\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) { var c Calc; c.Apply(1) }\n\nfunc TestTwo(t *testing.T) { var c Calc; c.Apply(2) }\n",
	}
	production := contextSamePkgPerSeed + 3
	for i := range production {
		files[fmt.Sprintf("p/c%02d.go", i)] = fmt.Sprintf("package p\n\nfunc Caller%02d() int { var c Calc; return c.Apply(%d) }\n", i, i)
	}
	s, _ := newSamePkgTool(t, files)
	args := map[string]any{"symbols": []string{"p/a.go#Calc.Apply"}, "intent": contextIntentChange}
	pack := s.collect(t, args)
	callers, _ := nameMatches(pack)
	if len(callers) != contextSamePkgPerSeed {
		t.Fatalf("%d callers listed, want the cap of %d: %v", len(callers), contextSamePkgPerSeed, callers)
	}
	for _, id := range callers {
		if strings.Contains(id, "_test.go") {
			t.Errorf("a test took a place from production code under the cap: %v", callers)
		}
	}
	if want := production + 2 - contextSamePkgPerSeed; pack.Expansion.Stats.SamePkgMore != want {
		t.Errorf("SamePkgMore = %d, want %d", pack.Expansion.Stats.SamePkgMore, want)
	}
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%d further same-package name match(es) beyond the per-declaration cap of %d were not listed", production+2-contextSamePkgPerSeed, contextSamePkgPerSeed); !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	requireNeverNoCallers(t, out)

	few := map[string]string{"p/a.go": files["p/a.go"], "p/c00.go": files["p/c00.go"], "p/c01.go": files["p/c01.go"]}
	f, _ := newSamePkgTool(t, few)
	pack = f.collect(t, args)
	if callers, _ := nameMatches(pack); len(callers) != 2 || pack.Expansion.Stats.SamePkgMore != 0 {
		t.Errorf("control: %v callers, %d cut; want both listed and none cut", callers, pack.Expansion.Stats.SamePkgMore)
	}
}

// The cap is the collector's, so a test (and nothing else) can name its own: with a
// cap of 2, four callers list two and count two.
func TestSamePackage_TheCapIsTheCollectorsOwn(t *testing.T) {
	files := map[string]string{"p/a.go": calcFile}
	for i := range 4 {
		files[fmt.Sprintf("p/c%d.go", i)] = fmt.Sprintf("package p\n\nfunc Caller%d() int { var c Calc; return c.Apply(%d) }\n", i, i)
	}
	s, _ := newSamePkgTool(t, files)
	s.collector.samePkgCap = 2
	pack := s.collect(t, map[string]any{"symbols": []string{"p/a.go#Calc.Apply"}})
	callers, _ := nameMatches(pack)
	requireIDs(t, "callers under a cap of 2", callers, goldID("p/c0.go", "Caller0"), goldID("p/c1.go", "Caller1"))
	if pack.Expansion.Stats.SamePkgMore != 2 {
		t.Errorf("SamePkgMore = %d, want 2", pack.Expansion.Stats.SamePkgMore)
	}
}

// A declaration the walk already holds (the subject's own recursion, a seed) takes no
// place under the cap: with a cap of one, the real caller is still the one listed.
func TestSamePackage_AHeldDeclarationSpendsNoPlaceUnderTheCap(t *testing.T) {
	s, _ := newSamePkgTool(t, map[string]string{
		"p/a.go": "package p\n\ntype Calc struct{}\n\nfunc (c *Calc) Apply(n int) int {\n\tif n < 0 {\n\t\treturn c.Apply(-n)\n\t}\n\treturn n\n}\n",
		"p/b.go": "package p\n\nfunc Savings(n int) int {\n\tvar c Calc\n\treturn n - c.Apply(n)\n}\n",
	})
	s.collector.samePkgCap = 1
	callers, _ := nameMatches(s.collect(t, map[string]any{"symbols": []string{"p/a.go#Calc.Apply"}}))
	requireIDs(t, "callers under a cap of 1", callers, goldID("p/b.go", "Savings"))
}

// A name that many tests call costs a bounded scan, and production code is scanned
// before tests, so it is the tests that fall outside the bound. The cut is counted and
// stated.
func TestSamePackage_ProductionCodeIsScannedBeforeTestsUnderTheScanBound(t *testing.T) {
	var tests strings.Builder
	tests.WriteString("package p\n\nimport \"testing\"\n\n")
	const extra = 7
	for i := range contextSamePkgSiteScan + extra - 1 {
		fmt.Fprintf(&tests, "func Test%03d(t *testing.T) { var c Calc; c.Apply(%d) }\n\n", i, i)
	}
	s, _ := newSamePkgTool(t, map[string]string{
		"p/a.go":      calcFile,
		"p/x_test.go": tests.String(),
		"p/z.go":      "package p\n\nfunc Last() int { var c Calc; return c.Apply(0) }\n",
	})
	s.collector.samePkgCap = 1000
	args := map[string]any{"symbols": []string{"p/a.go#Calc.Apply"}}
	pack := s.collect(t, args)
	callers, _ := nameMatches(pack)
	if !slices.Contains(callers, goldID("p/z.go", "Last")) {
		t.Errorf("the production caller fell outside the scan bound behind %d tests", contextSamePkgSiteScan+extra-1)
	}
	if pack.Expansion.Stats.SamePkgCut != extra {
		t.Errorf("SamePkgCut = %d, want %d", pack.Expansion.Stats.SamePkgCut, extra)
	}
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("same-package callers may be missing: %d call site(s) were beyond the scan bound of %d", extra, contextSamePkgSiteScan); !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

// Receivers of no evident type are gap candidates, at most contextGapPerSubject for
// one declaration, and the rest are counted.
func TestSamePackage_UnknownReceiversAreCappedAsGapCandidates(t *testing.T) {
	files := map[string]string{
		"p/t.go": "package p\n\ntype A struct{}\n\nfunc (a *A) Run() int { return 1 }\n\nfunc getA() *A { return &A{} }\n",
	}
	const total = contextGapPerSubject + 2
	for i := range total {
		files[fmt.Sprintf("p/g%02d.go", i)] = fmt.Sprintf("package p\n\nfunc g%02d() int { return getA().Run() }\n", i)
	}
	s, _ := newSamePkgTool(t, files)
	pack := s.collect(t, map[string]any{"symbols": []string{"p/t.go#A.Run"}})
	callers, cands := nameMatches(pack)
	if len(callers) != 0 || len(cands) != contextGapPerSubject || pack.Expansion.Stats.GapMore != total-contextGapPerSubject {
		t.Errorf("%d callers, %d candidates, %d cut; want 0, %d and %d", len(callers), len(cands), pack.Expansion.Stats.GapMore, contextGapPerSubject, total-contextGapPerSubject)
	}
}

// A type is not called: `Money(n)` is a conversion, and a bare call of that name must
// not make the function that converts a caller of the type.
func TestSamePackage_ATypeHasNoCallers(t *testing.T) {
	s, _ := newSamePkgTool(t, map[string]string{
		"p/money.go": "package p\n\ntype Money int\n",
		"p/conv.go":  "package p\n\nfunc Cents(n int) Money { return Money(n) }\n",
	})
	callers, cands := nameMatches(s.collect(t, map[string]any{"symbols": []string{"p/money.go#Money"}}))
	if len(callers)+len(cands) != 0 {
		t.Errorf("a conversion made a type's neighbour a caller: %v %v", callers, cands)
	}
}

// The sibling read stays under the agent's root and goes through the boundary guard:
// a symlink out of the root, a directory, an oversized file and a path the guard
// refuses are all unreadable; an ordinary file is read.
func TestSamePackage_TheSourceReaderStaysUnderTheRootAndThroughTheGuard(t *testing.T) {
	root, outside := canonicalRoot(t.TempDir()), canonicalRoot(t.TempDir())
	writeFiles(t, root, map[string]string{
		"a.go":       "package p\n",
		"refused.go": "package p\n",
		"big.go":     "package p\n" + strings.Repeat("// padding\n", contextSamePkgFileMax/10),
	})
	writeFiles(t, outside, map[string]string{"b.go": "package q\n"})
	if err := os.Symlink(filepath.Join(outside, "b.go"), filepath.Join(root, "link.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewContextCollector(nil).WithBoundary(func(_ context.Context, p string) error {
		if strings.HasSuffix(p, "refused.go") {
			return errors.New("refused by the guard")
		}
		return nil
	})
	read := c.sourceReader(context.Background(), root)
	if src, err := read("a.go"); err != nil || string(src) != "package p\n" {
		t.Fatalf("control: an ordinary file was not read: %q, %v", src, err)
	}
	for _, rel := range []string{"link.go", "dir.go", "big.go", "refused.go", "../b.go", "missing.go"} {
		if src, err := read(rel); err == nil {
			t.Errorf("%s was read (%d bytes)", rel, len(src))
		}
	}
}

// Each name of a directory is read once: a level plans one read per directory for the
// names not asked before, and nothing when every name was.
func TestSamePackage_EachNameOfADirectoryIsReadOnce(t *testing.T) {
	s, _ := newSamePkgTool(t, samePkgSiblingFiles)
	e := newExpander(s.store, contextScope{}, ranker{}, nil)
	e.sp = newSamePackage(nil, nil, 1)
	first := e.planRead("p", []string{"Apply", "Dup"})
	if first == nil || !slices.Equal(first.names, []string{"Apply", "Dup"}) {
		t.Fatalf("first plan = %+v, want both names", first)
	}
	second := e.planRead("p", []string{"Dup", "Other", "Apply"})
	if second == nil || !slices.Equal(second.names, []string{"Other"}) || second.ds != first.ds {
		t.Errorf("second plan = %+v, want only Other, into the same list", second)
	}
	if e.planRead("p", []string{"Apply", "Other"}) != nil {
		t.Error("a plan for names all asked before is not empty")
	}
	other := e.planRead("q", []string{"Apply"})
	if other == nil || other.ds == first.ds {
		t.Errorf("another directory shares a list: %+v", other)
	}
	// The reads of several directories at once fill each directory's own list.
	e.readSites(context.Background(), []*siteRead{first, other})
	if got := len(other.ds.byCallee["Apply"]); got != 1 {
		t.Errorf("q has %d recorded calls of Apply, want 1", got)
	}
	if got := len(first.ds.byCallee["Apply"]); got != 8 {
		t.Errorf("p has %d recorded calls of Apply, want 8", got)
	}
	if got := len(first.ds.byCallee["Dup"]); got != 0 {
		t.Errorf("p has %d calls of Dup, want none", got)
	}
}

// A read the deadline cut short is not an empty directory: the walk records that it ran
// out of time, and what it holds stays empty rather than half-filled.
func TestSamePackage_AReadCutByTheDeadlineIsRecordedNotMistakenForNothing(t *testing.T) {
	s, _ := newSamePkgTool(t, samePkgSiblingFiles)
	e := newExpander(s.store, contextScope{}, ranker{}, nil)
	e.sp = newSamePackage(nil, nil, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.readSites(ctx, []*siteRead{e.planRead("p", []string{"Apply"})})
	if !e.stats.Deadline || len(e.sp.dirs["p"].byCallee) != 0 {
		t.Errorf("after a cancelled read: Deadline=%v, %d callees held; want the deadline recorded and nothing held", e.stats.Deadline, len(e.sp.dirs["p"].byCallee))
	}
	e.prefetchSites(context.Background(), nil) // an empty level reads nothing
	var hint expander
	hint.prefetchSites(context.Background(), []hopNode{{node: topology.Node{Kind: topology.KindFunction, Name: "f", Path: "p/a.go"}}}) // a hint has no search
}

// A file is parsed only if the index holds a hash for it, the bytes read hash to
// exactly that, and the call's source budget has room for it.
func TestSamePackage_AFileIsTrustedOnlyWithinTheBudgetAndWhenItsHashMatches(t *testing.T) {
	src := []byte("package p\n")
	sum := sha256.Sum256(src)
	good := hex.EncodeToString(sum[:])
	load := func(budget int64, hash string) *goFile {
		sp := newSamePackage(func(string) ([]byte, error) { return src, nil }, func(string) string { return hash }, 1)
		sp.budget = budget
		return sp.load("x.go")
	}
	if load(int64(len(src)), good) == nil {
		t.Fatal("control: a file that fits and matches was not parsed")
	}
	for name, g := range map[string]*goFile{
		"over the budget":      load(int64(len(src))-1, good),
		"another hash":         load(1<<20, strings.Repeat("0", 64)),
		"no hash in the index": load(1<<20, ""),
	} {
		if g != nil {
			t.Errorf("a file %s was parsed", name)
		}
	}
}

// A sibling that changed since it was indexed is not trusted: its call sites' offsets
// describe other bytes. It is not read for callers, and the pack says so.
func TestSamePackage_ASiblingChangedSinceIndexingIsNotTrusted(t *testing.T) {
	s := newShop(t)
	args := map[string]any{"symbols": []string{"pricing/discount.go#Discount.Off"}}
	callers, _ := nameMatches(s.collect(t, args))
	if !slices.Contains(callers, goldID("pricing/receipt.go", "Tenth")) {
		t.Fatalf("control: the unchanged sibling is not listed: %v", callers)
	}

	file := filepath.Join(s.root, "pricing", "receipt.go")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append([]byte("// a line that moves every offset\n"), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	pack := s.collect(t, args)
	callers, _ = nameMatches(pack)
	if slices.Contains(callers, goldID("pricing/receipt.go", "Tenth")) {
		t.Errorf("a caller was taken from a sibling that changed since indexing: %v", callers)
	}
	if !slices.Contains(callers, goldID("pricing/discount_test.go", "TestOffPercent")) {
		t.Errorf("an unchanged sibling stopped being examined: %v", callers)
	}
	if pack.Expansion.Stats.SamePkgSkipped != 1 {
		t.Errorf("SamePkgSkipped = %d, want 1", pack.Expansion.Stats.SamePkgSkipped)
	}
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 sibling file(s) changed since indexing or could not be read") {
		t.Errorf("the unexamined sibling is not disclosed:\n%s", out)
	}
}
