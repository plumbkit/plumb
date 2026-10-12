package tools

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_affected_named_test.go — the named tests under an affected-package row
// (PLAN-543): only tests a call edge reaches, closest first, as a run_task filter.

// C01's cart tests reach Total only through a same-package NAME match, which is no
// call edge, so the cart row names none; C02's pricing test calls Apply through a
// resolved edge and is named (the control: the same fixture, the same section).
func TestContextForTask_Affected_NamesOnlyTestsACallEdgeReaches(t *testing.T) {
	s := newShop(t)
	_, out := runCase(t, s, oracle(t, "C01"))
	rows, sub := sectionRowsAndSubRows(out, affectedHeading)
	if len(rows) != 1 || !strings.HasPrefix(rows[0], "  ./cart/... — ") {
		t.Fatalf("control: C01 should have the one cart row, got %q", rows)
	}
	if len(sub[rows[0]]) != 0 {
		t.Errorf("tests no call edge reaches were named: %q\n%s", sub[rows[0]], out)
	}

	_, out = runCase(t, s, oracle(t, "C02"))
	rows, sub = sectionRowsAndSubRows(out, affectedHeading)
	var pricing string
	for _, r := range rows {
		if strings.HasPrefix(r, "  ./pricing/... — ") {
			pricing = r
		}
	}
	want := []string{`    run first: run_task(slot:"test", target:"./pricing/...", run:"^(TestApplyPercent)$") — 1 test(s) ` + namedTestsReach}
	if !slices.Equal(sub[pricing], want) {
		t.Errorf("pricing's named tests = %q, want %q\n%s", sub[pricing], want, out)
	}
	for _, r := range rows {
		if r != pricing && len(sub[r]) != 0 {
			t.Errorf("row %q names tests no call edge reaches: %q", r, sub[r])
		}
	}
}

// Without a Go package target there is no run_task filter to offer: the names
// stand alone, and the line still claims only the call edges.
func TestContextForTask_Affected_NamedTestsWithoutATargetAreJustNames(t *testing.T) {
	root := copyShopFixture(t)
	s := newShopTool(t, openShopStore(t, root), root)
	s.collector = s.collector.WithTestScope(func(context.Context) TestScope { return TestScope{} })
	s.tool = NewContextForTask(s.collector)
	_, out := runCase(t, s, oracle(t, "C06"))
	rows, sub := sectionRowsAndSubRows(out, affectedHeading)
	if len(rows) == 0 || !strings.HasPrefix(rows[0], "  pricing — ") {
		t.Fatalf("control: C06's first row should name the pricing directory, got %q\n%s", rows, out)
	}
	want := []string{fmt.Sprintf("    1 test(s) %s: TestApplyPercent", namedTestsReach)}
	if !slices.Equal(sub[rows[0]], want) {
		t.Errorf("named tests = %q, want %q", sub[rows[0]], want)
	}
}

func reachedTest(name string, conf float64, hops int) affectedTest {
	return affectedTest{Node: topology.Node{Name: name, Kind: topology.KindTest, Path: "p/x_test.go"}, Confidence: conf, Hops: hops}
}

// The naming rules, one at a time: edge-reached only, runnable names only, closest
// first with the name breaking ties and an unknown hop count last, one name per
// test, the per-package and package caps, and nothing at all from a stale index.
func TestEdgeReachedTests_Rules(t *testing.T) {
	pkg := affectedPackage{Dir: "p", Tests: []affectedTest{
		reachedTest("TestCoLocated", colocatedConfidence, 0), // no edge: never named
		reachedTest("TestFar", graphEdgeBaseline, 2),
		reachedTest("TestUnknownHops", graphEdgeBaseline, 0), // edge filtered from the subgraph
		reachedTest("TestNearB", 0.85, 1),
		reachedTest("TestNearA", 0.85, 1),
		reachedTest("TestNearA", 0.85, 1),       // the same test twice: named once
		reachedTest("BenchmarkNear", 0.85, 1),   // needs -bench, not -run
		reachedTest("Testify", 0.85, 1),         // a lowercase rune after Test: not a test
		reachedTest("helperNear", 0.85, 1),      // a helper is not a test
		reachedTest("Test", 0.85, 1),            // bare Test is a test
		reachedTest("ExampleNear", 0.85, 1),     // go test -run selects examples
		reachedTest("FuzzNear", 0.85, 1),        // and fuzz targets
		reachedTest("Test_underscore", 0.85, 1), // a non-letter after Test is fine
		reachedTest("TestÉclair", 0.85, 1),      // so is a non-lowercase letter
		reachedTest("TestNearC", 0.85, 1),       // ninth runnable: past the cap
		reachedTest("TestNearD", 0.85, 1),       // tenth
		reachedTest("TestCoLocatedToo", colocatedConfidence, 0),
	}}
	got := edgeReachedTests([]affectedPackage{pkg}, "fresh")["p"]
	// Byte order: "_" (0x5f) sorts before "É" (0xc3), so TestÉclair is the one cut.
	wantNames := []string{"ExampleNear", "FuzzNear", "Test", "TestNearA", "TestNearB", "TestNearC", "TestNearD", "Test_underscore"}
	if !slices.Equal(got.Names, wantNames) {
		t.Errorf("names = %q, want %q", got.Names, wantNames)
	}
	if got.Total != 11 {
		t.Errorf("total = %d, want 11 (9 one-hop runnable, TestFar, TestUnknownHops)", got.Total)
	}

	asideOne := reachedTest("TestAsideOne", 0.85, 1)
	asideOne.Aside = true // reaches only a callee of the seed: it does not run the seed
	far := edgeReachedTests([]affectedPackage{{Dir: "q", Tests: []affectedTest{
		asideOne, reachedTest("TestUnknown", graphEdgeBaseline, 0), reachedTest("TestThree", 0.85, 3), reachedTest("TestOne", 0.85, 1),
	}}}, "building")["q"]
	if want := []string{"TestOne", "TestThree", "TestUnknown", "TestAsideOne"}; !slices.Equal(far.Names, want) {
		t.Errorf("order = %q, want %q: tests that run a seed first, by hops, an aside test last however close "+
			"(and an index mid-build still names: its edges are real)", far.Names, want)
	}

	var many []affectedPackage
	for _, d := range []string{"a", "b", "c", "d"} {
		many = append(many, affectedPackage{Dir: d, Tests: []affectedTest{reachedTest("TestX", 0.85, 1)}})
	}
	many = append([]affectedPackage{{Dir: "none", Tests: []affectedTest{reachedTest("TestY", colocatedConfidence, 0)}}}, many...)
	if got := slices.Sorted(maps.Keys(edgeReachedTests(many, "fresh"))); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("packages named = %q, want the first %d that have any, in row order", got, contextNamedPackages)
	}

	if got := edgeReachedTests([]affectedPackage{pkg}, "stale"); len(got) != 0 {
		t.Errorf("a stale index named tests: %v", got)
	}
}

// The run filter must be one run_task accepts: names are dropped from the end
// until it fits, and the count says how many are left out.
func TestNamedTestsLine_FilterFitsWhatRunTaskAccepts(t *testing.T) {
	names := make([]string, 0, contextNamedPerPackage)
	for i := range contextNamedPerPackage {
		// Distinct after "Test0", so the shared prefix saves almost nothing.
		names = append(names, fmt.Sprintf("Test%02dAVeryLongNameThatDescribesBehaviourInDetail", i))
	}
	line := namedTestsLine(TestScope{Language: "go", Style: TargetGoPackage}, "pkg", namedTests{Names: names, Total: 20})
	_, rest, ok := strings.Cut(line, `run:"`)
	if !ok {
		t.Fatalf("no run filter in %q", line)
	}
	filter, _, _ := strings.Cut(rest, `")`)
	if err := validateRunFilter("run", filter); err != nil {
		t.Fatalf("the offered filter is one run_task refuses: %v\n%s", err, line)
	}
	kept := strings.Count(filter, "|") + 1
	if kept >= len(names) || kept == 0 {
		t.Fatalf("control: %d names should not all fit in the filter limit, kept %d", len(names), kept)
	}
	if want := fmt.Sprintf("%d of 20 test(s) that %s; %d more are not named", kept, namedTestsReach, 20-kept); !strings.HasSuffix(line, want) {
		t.Errorf("line = %q,\nwant the suffix %q", line, want)
	}
}

// The filter writes the common prefix once and selects exactly the names: go
// test's own matcher (one part, since the alternation sits in a group) accepts
// each name and rejects a near miss.
func TestRunFilterFor(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  string
		miss  string
	}{
		{[]string{"TestMoveSymbol_Refusals", "TestMoveSymbol_CreateDestination"}, "^TestMoveSymbol_(Refusals|CreateDestination)$", "TestMoveSymbol_Refusals2"},
		{[]string{"TestOne"}, "^(TestOne)$", "TestOneMore"},
		{[]string{"TestA", "TestAB"}, "^TestA(|B)$", "TestABC"},
		{[]string{"ExampleX", "TestX"}, "^(ExampleX|TestX)$", "TestXY"},
		{[]string{"TestÉa", "TestÈb"}, "^Test(Éa|Èb)$", "TestÉb"}, // É and È share their first byte
	} {
		got := runFilterFor(tc.names)
		if got != tc.want {
			t.Errorf("runFilterFor(%q) = %q, want %q", tc.names, got, tc.want)
			continue
		}
		re := regexp.MustCompile(got)
		for _, n := range tc.names {
			if !re.MatchString(n) {
				t.Errorf("%q does not select %q", got, n)
			}
		}
		if re.MatchString(tc.miss) {
			t.Errorf("%q also selects %q", got, tc.miss)
		}
	}
}

// A test reached twice keeps the shorter path of the same kind. The walk records
// paths that run the change before aside ones, so neither kind replaces the other.
func TestBetterPath(t *testing.T) {
	seed2 := affectedTest{Hops: 2}
	aside3 := affectedTest{Hops: 3, Aside: true}
	for _, tc := range []struct {
		name  string
		have  affectedTest
		hops  int
		aside bool
		want  bool
	}{
		{"an aside path never replaces one that runs the change", seed2, 1, true, false},
		{"nor the other way: the walk never records the aside one first", aside3, 1, false, false},
		{"fewer hops win", seed2, 1, false, true},
		{"fewer hops win among aside paths too", aside3, 2, true, true},
		{"equal hops do not", seed2, 2, false, false},
		{"more hops do not", seed2, 3, false, false},
		{"any count beats an unknown one", affectedTest{}, 4, false, true},
	} {
		if got := betterPath(tc.have, tc.hops, tc.aside); got != tc.want {
			t.Errorf("%s: betterPath = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A test reached from two roots keeps the shorter path; nodes the edges do not
// connect to the root have no hop count.
func TestInwardHops(t *testing.T) {
	const root, a, b, c, loose = 1, 2, 3, 4, 5
	edges := []topology.Edge{
		{FromID: a, ToID: root}, // a depends on root: 1 hop
		{FromID: b, ToID: a},    // b depends on a: 2 hops
		{FromID: c, ToID: b},    // c: 3 hops through b ...
		{FromID: c, ToID: root}, // ... but 1 directly
		{FromID: loose, ToID: 99},
	}
	got := inwardHops(root, edges)
	want := map[int64]int{a: 1, b: 2, c: 1}
	if !maps.Equal(got, want) {
		t.Errorf("inwardHops = %v, want %v", got, want)
	}
}
