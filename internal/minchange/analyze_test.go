package minchange

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// newFileDiff builds a unified diff that creates path with the given lines (all
// added), the shape git emits for a brand-new file.
func newFileDiff(path string, lines ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n", path, path)
	b.WriteString("new file mode 100644\nindex 0000000..1111111\n")
	fmt.Fprintf(&b, "--- /dev/null\n+++ b/%s\n", path)
	fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", len(lines))
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

// modifiedDiff builds a unified diff that adds addedLines into an existing file
// after one line of context.
func modifiedDiff(path string, addedLines ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", path, path, path, path)
	fmt.Fprintf(&b, "@@ -1,1 +1,%d @@\n context line\n", len(addedLines)+1)
	for _, l := range addedLines {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

func kinds(r Report) map[string]int {
	m := map[string]int{}
	for _, f := range r.Findings {
		m[f.Kind]++
	}
	return m
}

func TestThinWrapper_DetectsPassthrough(t *testing.T) {
	diff := ParseUnifiedDiff(newFileDiff("pkg/wrap.go",
		"package pkg",
		"",
		"func Wrap(a int, b string) error {",
		"\treturn Target(a, b)",
		"}",
	))
	r := Analyze(context.Background(), diff, Deps{}, Options{IncludeSuggestions: true})
	if kinds(r)[KindThinWrapper] != 1 {
		t.Fatalf("want 1 thin-wrapper finding, got %d (%+v)", kinds(r)[KindThinWrapper], r.Findings)
	}
	f := findingOf(r, KindThinWrapper)
	if f.Severity != Warning || f.Confidence != High {
		t.Errorf("thin wrapper severity/confidence = %s/%s, want warning/high", f.Severity, f.Confidence)
	}
	if !strings.Contains(f.Evidence, "Target(a, b)") {
		t.Errorf("evidence lacks the forwarded call: %q", f.Evidence)
	}
	if f.Alternative == "" {
		t.Errorf("want a smaller-alternative suggestion")
	}
}

func TestThinWrapper_QuietOnTransformingBody(t *testing.T) {
	// The wrapper adds 1 to the argument — not a pure passthrough.
	diff := ParseUnifiedDiff(newFileDiff("pkg/wrap.go",
		"package pkg",
		"func Wrap(a int) int {",
		"\treturn Target(a + 1)",
		"}",
	))
	r := Analyze(context.Background(), diff, Deps{}, Options{})
	if kinds(r)[KindThinWrapper] != 0 {
		t.Errorf("transforming wrapper wrongly flagged: %+v", r.Findings)
	}
}

func TestThinWrapper_QuietOnRealMultiStatementBody(t *testing.T) {
	diff := ParseUnifiedDiff(newFileDiff("pkg/real.go",
		"package pkg",
		"func Real(a int) error {",
		"\tx := compute(a)",
		"\treturn store(x)",
		"}",
	))
	r := Analyze(context.Background(), diff, Deps{}, Options{})
	if kinds(r)[KindThinWrapper] != 0 {
		t.Errorf("multi-statement function wrongly flagged: %+v", r.Findings)
	}
}

func TestSingleUse_FlagsExactlyOneCaller(t *testing.T) {
	diff := ParseUnifiedDiff(newFileDiff("pkg/help.go",
		"package pkg",
		"func Helper(x int) int {",
		"\treturn x * 2",
		"}",
	))
	deps := Deps{
		CallerCount: func(_ context.Context, name string) (int, SymbolRef, bool) {
			if name == "Helper" {
				return 1, SymbolRef{Name: "OnlyCaller", Path: "pkg/help.go", Line: 42}, true
			}
			return 0, SymbolRef{}, false
		},
	}
	r := Analyze(context.Background(), diff, deps, Options{IncludeSuggestions: true})
	f := findingOf(r, KindSingleUse)
	if f == nil {
		t.Fatalf("want a single-use finding, got %+v", r.Findings)
	}
	if f.Confidence != Low {
		t.Errorf("single-use confidence = %s, want low (approximate)", f.Confidence)
	}
	if !strings.Contains(f.Evidence, "pkg/help.go:42") || !strings.Contains(f.Evidence, "cross-file") {
		t.Errorf("single-use evidence must cite the site AND caveat cross-file: %q", f.Evidence)
	}
}

func TestSingleUse_QuietWhenMultipleOrAbsentCallers(t *testing.T) {
	diff := ParseUnifiedDiff(newFileDiff("pkg/help.go",
		"package pkg",
		"func Helper() {}",
	))
	cases := map[string]func(context.Context, string) (int, SymbolRef, bool){
		"three callers": func(context.Context, string) (int, SymbolRef, bool) { return 3, SymbolRef{}, true },
		"not in index":  func(context.Context, string) (int, SymbolRef, bool) { return 0, SymbolRef{}, false },
		"zero callers":  func(context.Context, string) (int, SymbolRef, bool) { return 0, SymbolRef{}, true },
	}
	for name, cc := range cases {
		t.Run(name, func(t *testing.T) {
			r := Analyze(context.Background(), diff, Deps{CallerCount: cc}, Options{})
			if kinds(r)[KindSingleUse] != 0 {
				t.Errorf("%s: single-use wrongly flagged", name)
			}
		})
	}
}

func TestDuplicateHelper_FlagsCloseNameMatch(t *testing.T) {
	diff := ParseUnifiedDiff(newFileDiff("pkg/new.go",
		"package pkg",
		"func parseConfig(b []byte) error {",
		"\treturn nil",
		"}",
	))
	deps := Deps{
		SimilarSymbols: func(_ context.Context, name, exclude string) []SymbolRef {
			if name == "parseConfig" && exclude == "pkg/new.go" {
				return []SymbolRef{{Name: "parseConfigs", Path: "other/cfg.go", Line: 9, Kind: "function"}}
			}
			return nil
		},
	}
	r := Analyze(context.Background(), diff, deps, Options{IncludeSuggestions: true})
	f := findingOf(r, KindDuplicateHelper)
	if f == nil {
		t.Fatalf("want a duplicate-helper finding, got %+v", r.Findings)
	}
	if f.Confidence != Low {
		t.Errorf("duplicate-helper confidence = %s, want low", f.Confidence)
	}
	if !strings.Contains(f.Evidence, "other/cfg.go:9") {
		t.Errorf("evidence should cite the resembling symbol: %q", f.Evidence)
	}
}

func TestDependency_FlagsCuratedStdlibEquivalent(t *testing.T) {
	raw := "diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n" +
		"@@ -3,2 +3,3 @@\n require (\n \tgithub.com/existing/dep v1.0.0\n" +
		"+\tgithub.com/pkg/errors v0.9.1\n"
	r := Analyze(context.Background(), ParseUnifiedDiff(raw), Deps{}, Options{IncludeSuggestions: true})
	f := findingOf(r, KindStdlibCandidate)
	if f == nil {
		t.Fatalf("want a stdlib-candidate finding, got %+v", r.Findings)
	}
	if f.Severity != Info {
		t.Errorf("stdlib-candidate must be info (never stronger), got %s", f.Severity)
	}
	if !strings.Contains(f.Evidence, "github.com/pkg/errors") {
		t.Errorf("evidence should name the added dep: %q", f.Evidence)
	}
	if !strings.Contains(f.Alternative, "%w") {
		t.Errorf("alternative should point at the stdlib path: %q", f.Alternative)
	}
}

func TestDependency_QuietOnVersionBump(t *testing.T) {
	// Same module removed and re-added at a new version — not a new dependency.
	raw := "diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n" +
		"@@ -3,2 +3,2 @@\n require (\n-\tgithub.com/pkg/errors v0.8.0\n" +
		"+\tgithub.com/pkg/errors v0.9.1\n"
	r := Analyze(context.Background(), ParseUnifiedDiff(raw), Deps{}, Options{})
	if kinds(r)[KindStdlibCandidate] != 0 {
		t.Errorf("version bump wrongly flagged as a new dependency: %+v", r.Findings)
	}
}

func TestDependency_QuietOnUncuratedModule(t *testing.T) {
	raw := "diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n" +
		"@@ -3,1 +3,2 @@\n require (\n+\tgithub.com/some/legit-dep v1.2.3\n"
	r := Analyze(context.Background(), ParseUnifiedDiff(raw), Deps{}, Options{})
	if kinds(r)[KindStdlibCandidate] != 0 {
		t.Errorf("uncurated module wrongly flagged: %+v", r.Findings)
	}
}

func TestDependency_QuietOnIndirectRequire(t *testing.T) {
	// An "// indirect" require is added by go mod tidy, not chosen by the
	// author — never a dependency decision to review.
	raw := "diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n" +
		"@@ -3,1 +3,2 @@\n require (\n+\tgithub.com/pkg/errors v0.9.1 // indirect\n"
	r := Analyze(context.Background(), ParseUnifiedDiff(raw), Deps{}, Options{})
	if kinds(r)[KindStdlibCandidate] != 0 {
		t.Errorf("indirect require wrongly flagged: %+v", r.Findings)
	}
}

func TestVerificationGap_FlagsSourceChangeWithoutTest(t *testing.T) {
	diff := ParseUnifiedDiff(modifiedDiff("internal/x/logic.go",
		"\tif newCondition {",
		"\t\tdoNewThing()",
		"\t}",
	))
	r := Analyze(context.Background(), diff, Deps{}, Options{IncludeSuggestions: true})
	f := findingOf(r, KindVerificationGap)
	if f == nil {
		t.Fatalf("want a verification-gap finding, got %+v", r.Findings)
	}
	if f.Severity != Warning || f.Confidence != High {
		t.Errorf("verification-gap should be warning/high, got %s/%s", f.Severity, f.Confidence)
	}
	if !strings.Contains(f.Alternative, "topology_affected") || !strings.Contains(f.Alternative, "run_task") {
		t.Errorf("alternative should recommend the concrete follow-up calls: %q", f.Alternative)
	}
}

func TestVerificationGap_QuietWhenTestChangedToo(t *testing.T) {
	diff := ParseUnifiedDiff(
		modifiedDiff("internal/x/logic.go", "\tdoNewThing()") +
			modifiedDiff("internal/x/logic_test.go", "\tassertNewThing()"),
	)
	r := Analyze(context.Background(), diff, Deps{}, Options{})
	if kinds(r)[KindVerificationGap] != 0 {
		t.Errorf("verification-gap wrongly flagged when a test changed: %+v", r.Findings)
	}
}

func TestVerificationGap_QuietOnDocsOnlyChange(t *testing.T) {
	diff := ParseUnifiedDiff(modifiedDiff("README.md", "a new sentence."))
	r := Analyze(context.Background(), diff, Deps{}, Options{})
	if kinds(r)[KindVerificationGap] != 0 {
		t.Errorf("docs-only change wrongly flagged: %+v", r.Findings)
	}
}

func TestAnalyze_BoundsFindingsAndReportsTruncation(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString(newFileDiff(fmt.Sprintf("pkg/w%d.go", i),
			"package pkg",
			fmt.Sprintf("func Wrap%d(a int) int {", i),
			fmt.Sprintf("\treturn Target%d(a)", i),
			"}",
		))
	}
	r := Analyze(context.Background(), ParseUnifiedDiff(b.String()), Deps{}, Options{MaxFindings: 3})
	if len(r.Findings) != 3 {
		t.Fatalf("want findings capped at 3, got %d", len(r.Findings))
	}
	if !r.Truncated {
		t.Errorf("Truncated should be set when findings are dropped")
	}
}

func TestAnalyze_IncludeSuggestionsFalseStripsAlternatives(t *testing.T) {
	diff := ParseUnifiedDiff(newFileDiff("pkg/wrap.go",
		"package pkg",
		"func Wrap(a int) int {",
		"\treturn Target(a)",
		"}",
	))
	r := Analyze(context.Background(), diff, Deps{}, Options{IncludeSuggestions: false})
	for _, f := range r.Findings {
		if f.Alternative != "" {
			t.Errorf("Alternative should be empty when suggestions are off: %q", f.Alternative)
		}
	}
}

func TestAnalyze_NotCheckedReportsTopologyAndScopeBlindSpots(t *testing.T) {
	diff := ParseUnifiedDiff(newFileDiff("pkg/x.go", "package pkg"))
	// No topology deps, no file scope.
	r := Analyze(context.Background(), diff, Deps{}, Options{ScopedToFiles: false})
	joined := strings.Join(r.NotChecked, "\n")
	if !strings.Contains(joined, "topology index unavailable") {
		t.Errorf("NotChecked should note the missing topology checks: %v", r.NotChecked)
	}
	if !strings.Contains(joined, "entire working-tree diff") {
		t.Errorf("NotChecked should warn about unscoped review: %v", r.NotChecked)
	}
}

func findingOf(r Report, kind string) *Finding {
	for i := range r.Findings {
		if r.Findings[i].Kind == kind {
			return &r.Findings[i]
		}
	}
	return nil
}
