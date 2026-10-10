package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// context_affected_test.go — the compact test-impact estimate (PLAN-462 A4),
// measured against the gold's test_packages and test_package_counts.

// sectionRows are the two-space rows under heading in a rendered pack, up to the
// next line that is not one.
func sectionRows(out, heading string) []string {
	var rows []string
	in := false
	for _, l := range strings.Split(out, "\n") {
		switch {
		case l == heading:
			in = true
		case in && strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   "):
			rows = append(rows, l)
		case in:
			return rows
		}
	}
	return rows
}

// goldCounts reads a gold key that maps names to counts.
func (c oracleCase) goldCounts(t *testing.T, key string) map[string]int {
	t.Helper()
	raw, ok := c.Gold[key]
	if !ok {
		t.Fatalf("%s has no gold.%s", c.ID, key)
	}
	var m map[string]int
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s gold.%s: %v", c.ID, key, err)
	}
	return m
}

// affectedTargets is the package column of the affected-tests rows: what precedes
// the first " — ".
func affectedTargets(rows []string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		target, _, _ := strings.Cut(strings.TrimPrefix(r, "  "), " — ")
		out = append(out, target)
	}
	return out
}

// executionWords would claim a test was run. Nothing in the estimate may say one.
var executionWords = regexp.MustCompile(`(?i)\b(passed|passes|failed|fails|green|ran|results?|succeeded)\b`)

// C07: a broad test package is one row with its count, never its individual tests,
// and nothing in the estimate says a test was run (Invariant 10).
func TestContextForTask_Affected_C07_OneRowWithTheCountAndNoTestNames(t *testing.T) {
	s := newShop(t)
	c := oracle(t, "C07")
	_, out := runCase(t, s, c)

	rows := sectionRows(out, affectedHeading)
	counts := c.goldCounts(t, "test_package_counts")
	want := []string{fmt.Sprintf("  ./reports/... — %d test(s); holds a seed", counts["./reports/..."])}
	if !slices.Equal(rows, want) {
		t.Fatalf("affected rows = %q, want exactly %q", rows, want)
	}
	if strings.Contains(out, "TestSummariseCase") {
		t.Errorf("forbidden: individual tests are listed in the compact pack:\n%s", out)
	}
	for _, line := range append([]string{affectedHeading}, rows...) {
		if m := executionWords.FindString(line); m != "" {
			t.Errorf("the estimate implies a test was run (%q): %q", m, line)
		}
	}
	for _, must := range []string{"static estimate", "no test was run"} {
		if !strings.Contains(affectedHeading, must) {
			t.Errorf("the heading does not say %q", must)
		}
	}
}

// The gold's test packages, as a set: seeds' own package first, then the packages a
// neighbour puts in play, each with the reason it is implicated.
func TestContextForTask_Affected_GoldPackagesAndReasons(t *testing.T) {
	for _, tc := range []struct {
		id      string
		reasons map[string]string
		first   string
	}{
		{"C01", map[string]string{"./cart/...": "holds a seed"}, "./cart/..."},
		{"C02", map[string]string{"./cart/...": "holds a seed", "./pricing/...": "holds a declaration next to a seed"}, "./cart/..."},
		{"C06", map[string]string{"./pricing/...": "holds a seed", "./cart/...": "holds a declaration next to a seed"}, "./pricing/..."},
	} {
		t.Run(tc.id, func(t *testing.T) {
			s := newShop(t)
			c := oracle(t, tc.id)
			_, out := runCase(t, s, c)
			rows := sectionRows(out, affectedHeading)
			gold := c.gold(t, "test_packages")
			if got := affectedTargets(rows); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(gold))) {
				t.Fatalf("affected packages = %v, want the gold %v\n%s", got, gold, out)
			}
			if got := affectedTargets(rows)[0]; got != tc.first {
				t.Errorf("first row is %s, want the seed's own package %s", got, tc.first)
			}
			for _, r := range rows {
				target, rest, _ := strings.Cut(strings.TrimPrefix(r, "  "), " — ")
				if !strings.HasSuffix(rest, "; "+tc.reasons[target]) {
					t.Errorf("row %q should end with the reason %q", r, tc.reasons[target])
				}
			}
		})
	}
}

// Without the calling agent's test command, a package is named by its directory and
// no command is guessed, as topology_affected does.
func TestContextForTask_Affected_NamesDirectoriesWhenNoTestCommandIsKnown(t *testing.T) {
	s := newShop(t)
	s.collector.testScope = nil
	out, err := s.run(t, oracle(t, "C07").args())
	if err != nil {
		t.Fatal(err)
	}
	if rows := sectionRows(out, affectedHeading); len(rows) != 1 || !strings.HasPrefix(rows[0], "  reports — 60 test(s)") {
		t.Errorf("rows = %q, want the directory, not a target", rows)
	}
}

// Scope narrows the estimate: a package outside within is left out and counted,
// never named. Control: without within the same call names it.
func TestContextForTask_Affected_WithinNarrowsAndTheGapCountsWhatItDropped(t *testing.T) {
	s := newShop(t)
	args := oracle(t, "C06").args()
	_, open := runCase(t, s, oracle(t, "C06"))
	if got := affectedTargets(sectionRows(open, affectedHeading)); len(got) != 2 {
		t.Fatalf("control: want two packages without within, got %v", got)
	}

	args["within"] = []string{"pricing"}
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	rows := sectionRows(out, affectedHeading)
	if got := affectedTargets(rows); !slices.Equal(got, []string{"./pricing/..."}) {
		t.Fatalf("affected packages = %v, want only ./pricing/...\n%s", got, out)
	}
	if strings.Contains(out, "./cart/...") {
		t.Errorf("a package outside within was named:\n%s", out)
	}
	if !strings.Contains(out, "affected tests: 2 test(s) outside within/corpora were left out of the estimate") {
		t.Errorf("the pack does not count what within dropped:\n%s", out)
	}
}

// A seed that is a document brings no code to estimate from: no rows, and no
// "none found" disclaimer about code nobody asked about.
func TestContextForTask_Affected_ADocumentSeedHasNothingToEstimate(t *testing.T) {
	s := newShop(t)
	out, err := s.run(t, oracle(t, "C13").args())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, affectedHeading) || strings.Contains(out, "affected tests:") {
		t.Errorf("a document seed produced an affected-tests section or disclaimer:\n%s", out)
	}
}

// When the budget is short the estimate goes before the follow-up calls and is
// counted by class in the footer.
func TestContextForTask_Affected_IsDroppedAndCountedWhenTheBudgetIsShort(t *testing.T) {
	s := newShop(t)
	args := oracle(t, "C07").args()
	args["max_bytes"] = contextMinMaxBytes
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "./reports/...") {
		t.Fatalf("control: the estimate fit in the smallest budget:\n%s", out)
	}
	got, _, hasFooter := parseFooter(t, out)
	if !hasFooter || got[classAffected] < 2 {
		t.Errorf("footer counts %v: the heading and the row of the dropped estimate must be counted as affected", got)
	}
}

// With no way to reach the collector's graph step (no workspace attached) nothing
// is estimated.
func TestContextForTask_Affected_NeedsAUsableIndex(t *testing.T) {
	root := copyShopFixture(t)
	col := NewContextCollector(nil).WithWorkspace(func(context.Context) string { return root }).WithBoundary(testBoundaryGuard(root))
	raw, _ := json.Marshal(map[string]any{"files": []string{"reports/reports.go"}, "intent": contextIntentChange})
	out, err := NewContextForTask(col).Execute(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, affectedHeading) {
		t.Errorf("an absent index still produced an estimate:\n%s", out)
	}
}
