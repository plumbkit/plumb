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

// renamedDiff builds a unified diff for a file renamed from oldPath to
// newPath, carrying the similarity/rename extended headers plus a
// comment-only content tweak (so it exercises the rename path alongside a
// hunk without itself constituting a logic change).
func renamedDiff(oldPath, newPath string) string {
	return fmt.Sprintf(
		"diff --git a/%s b/%s\nsimilarity index 88%%\nrename from %s\nrename to %s\nindex 1111111..2222222 100644\n--- a/%s\n+++ b/%s\n@@ -1,2 +1,3 @@\n package pkg\n+// renamed, no logic change\n func Foo() {}\n",
		oldPath, newPath, oldPath, newPath, oldPath, newPath)
}

// binaryOnlyDiff builds a unified diff whose only file change is a binary
// file (no textual hunk to analyse).
func binaryOnlyDiff(path string) string {
	return fmt.Sprintf("diff --git a/%s b/%s\nindex aaa..bbb 100644\nBinary files a/%s and b/%s differ\n", path, path, path, path)
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
	r := Analyse(context.Background(), diff, Deps{}, Options{IncludeSuggestions: true})
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
	r := Analyse(context.Background(), diff, deps, Options{IncludeSuggestions: true})
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

func TestSingleUse_PathAwareCallerKeepsLowConfidence(t *testing.T) {
	diff := ParseUnifiedDiff(newFileDiff("internal/new/helper.go",
		"package helper",
		"func Helper() {}",
	))
	var gotName, gotPath, gotKind string
	r := Analyse(context.Background(), diff, Deps{
		CallerCountAt: func(_ context.Context, name, path, kind string) (int, SymbolRef, bool) {
			gotName, gotPath, gotKind = name, path, kind
			return 1, SymbolRef{Name: "Caller", Path: "internal/caller/caller.go", Line: 9}, true
		},
	}, Options{})
	f := findingOf(r, KindSingleUse)
	if f == nil || f.Confidence != Low {
		t.Fatalf("path-aware single-use finding = %+v, want low-confidence finding", f)
	}
	if gotName != "Helper" || gotPath != "internal/new/helper.go" || gotKind != "function" {
		t.Fatalf("path-aware callback received (%q, %q, %q), want the changed symbol identity", gotName, gotPath, gotKind)
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
			r := Analyse(context.Background(), diff, Deps{CallerCount: cc}, Options{})
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
	r := Analyse(context.Background(), diff, deps, Options{IncludeSuggestions: true})
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
	r := Analyse(context.Background(), ParseUnifiedDiff(raw), Deps{}, Options{IncludeSuggestions: true})
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

func TestVerificationGap_SkippedOnTruncatedDiff(t *testing.T) {
	// A byte-truncated diff may have cut off the test change that would keep
	// this check quiet: it must not claim High-confidence "unverified", and
	// the skip must be disclosed.
	diff := ParseUnifiedDiff(modifiedDiff("internal/x/logic.go",
		"\tif newCondition {",
		"\tif oldCondition {"))
	r := Analyse(context.Background(), diff, Deps{}, Options{DiffTruncated: true})
	if kinds(r)[KindVerificationGap] != 0 {
		t.Errorf("truncated diff must not produce a verification-gap finding: %+v", r.Findings)
	}
	disclosed := false
	for _, n := range r.NotChecked {
		if strings.Contains(n, "truncated") {
			disclosed = true
		}
	}
	if !disclosed {
		t.Errorf("the truncation skip must be disclosed in NotChecked, got %v", r.NotChecked)
	}
}

func TestVerificationGap_FlagsSourceChangeWithoutTest(t *testing.T) {
	diff := ParseUnifiedDiff(modifiedDiff("internal/x/logic.go",
		"\tif newCondition {",
		"\t\tdoNewThing()",
		"\t}",
	))
	r := Analyse(context.Background(), diff, Deps{}, Options{IncludeSuggestions: true})
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

// TestQuiet_NoFindingForBenignChanges collapses the package's "quiet" cases
// (a check that must NOT fire) into one table: each case builds a diff and
// asserts the given finding kind's count is zero (or, when kind is "", that
// no finding at all was produced) after Analyse runs with no injected
// topology Deps.
func TestQuiet_NoFindingForBenignChanges(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		kind string // finding kind that must stay at zero; "" means zero findings total
	}{
		{
			// The wrapper adds 1 to the argument — not a pure passthrough.
			name: "thin wrapper stays quiet on a transforming body",
			raw: newFileDiff("pkg/wrap.go",
				"package pkg",
				"func Wrap(a int) int {",
				"\treturn Target(a + 1)",
				"}",
			),
			kind: KindThinWrapper,
		},
		{
			name: "thin wrapper stays quiet on a real multi-statement body",
			raw: newFileDiff("pkg/real.go",
				"package pkg",
				"func Real(a int) error {",
				"\tx := compute(a)",
				"\treturn store(x)",
				"}",
			),
			kind: KindThinWrapper,
		},
		{
			// Same module removed and re-added at a new version — not a new dependency.
			name: "dependency stays quiet on a version bump",
			raw: "diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n" +
				"@@ -3,2 +3,2 @@\n require (\n-\tgithub.com/pkg/errors v0.8.0\n" +
				"+\tgithub.com/pkg/errors v0.9.1\n",
			kind: KindStdlibCandidate,
		},
		{
			name: "dependency stays quiet on an uncurated module",
			raw: "diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n" +
				"@@ -3,1 +3,2 @@\n require (\n+\tgithub.com/some/legit-dep v1.2.3\n",
			kind: KindStdlibCandidate,
		},
		{
			// An "// indirect" require is added by go mod tidy, not chosen by the
			// author — never a dependency decision to review.
			name: "dependency stays quiet on an indirect require",
			raw: "diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n" +
				"@@ -3,1 +3,2 @@\n require (\n+\tgithub.com/pkg/errors v0.9.1 // indirect\n",
			kind: KindStdlibCandidate,
		},
		{
			// A doc-comment or licence-header edit is not "source logic changed" —
			// the tool's strongest warning must not fire on it.
			name: "verification gap stays quiet on a comment-only change",
			raw: "diff --git a/internal/x/logic.go b/internal/x/logic.go\n" +
				"--- a/internal/x/logic.go\n+++ b/internal/x/logic.go\n" +
				"@@ -1,2 +1,4 @@\n package x\n+// Package x resolves widgets.\n+\n func F() {}\n",
			kind: KindVerificationGap,
		},
		{
			name: "verification gap stays quiet when a test changed too",
			raw: modifiedDiff("internal/x/logic.go", "\tdoNewThing()") +
				modifiedDiff("internal/x/logic_test.go", "\tassertNewThing()"),
			kind: KindVerificationGap,
		},
		{
			name: "verification gap stays quiet on a docs-only change",
			raw:  modifiedDiff("README.md", "a new sentence."),
			kind: KindVerificationGap,
		},
		{
			// A binary file has no textual diff to analyse — it must not produce
			// any finding, on any check.
			name: "quiet on a binary-only file",
			raw:  binaryOnlyDiff("logo.png"),
			kind: "",
		},
		{
			// A rename with a comment-only content tweak must not be misread as a
			// logic change just because the paths differ (OldPath vs Path).
			name: "verification gap stays quiet on a rename with a comment-only tweak",
			raw:  renamedDiff("pkg/old.go", "pkg/new.go"),
			kind: KindVerificationGap,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Analyse(context.Background(), ParseUnifiedDiff(c.raw), Deps{}, Options{})
			if c.kind == "" {
				if len(r.Findings) != 0 {
					t.Errorf("want no findings at all, got %+v", r.Findings)
				}
				return
			}
			if kinds(r)[c.kind] != 0 {
				t.Errorf("%s wrongly flagged: %+v", c.kind, r.Findings)
			}
		})
	}
}

// TestAnalyse_NotCheckedFlagsBinaryFiles asserts that a diff containing a
// binary FileDiff produces the "binary files were skipped" NotChecked entry,
// so the report discloses the blind spot rather than reading as clean by
// omission.
func TestAnalyse_NotCheckedFlagsBinaryFiles(t *testing.T) {
	r := Analyse(context.Background(), ParseUnifiedDiff(binaryOnlyDiff("logo.png")), Deps{}, Options{})
	found := false
	for _, n := range r.NotChecked {
		if strings.Contains(n, "binary files were skipped") {
			found = true
		}
	}
	if !found {
		t.Errorf("NotChecked should disclose skipped binary files, got %v", r.NotChecked)
	}
}

func TestAnalyse_BoundsFindingsAndReportsTruncation(t *testing.T) {
	var b strings.Builder
	for i := range 10 {
		b.WriteString(newFileDiff(fmt.Sprintf("pkg/w%d.go", i),
			"package pkg",
			fmt.Sprintf("func Wrap%d(a int) int {", i),
			fmt.Sprintf("\treturn Target%d(a)", i),
			"}",
		))
	}
	r := Analyse(context.Background(), ParseUnifiedDiff(b.String()), Deps{}, Options{MaxFindings: 3})
	if len(r.Findings) != 3 {
		t.Fatalf("want findings capped at 3, got %d", len(r.Findings))
	}
	if !r.Truncated {
		t.Errorf("Truncated should be set when findings are dropped")
	}
}

func TestAnalyse_IncludeSuggestionsFalseStripsAlternatives(t *testing.T) {
	diff := ParseUnifiedDiff(newFileDiff("pkg/wrap.go",
		"package pkg",
		"func Wrap(a int) int {",
		"\treturn Target(a)",
		"}",
	))
	r := Analyse(context.Background(), diff, Deps{}, Options{IncludeSuggestions: false})
	for _, f := range r.Findings {
		if f.Alternative != "" {
			t.Errorf("Alternative should be empty when suggestions are off: %q", f.Alternative)
		}
	}
}

func TestAnalyse_NotCheckedReportsTopologyAndScopeBlindSpots(t *testing.T) {
	diff := ParseUnifiedDiff(newFileDiff("pkg/x.go", "package pkg"))
	// No topology deps, no file scope.
	r := Analyse(context.Background(), diff, Deps{}, Options{ScopedToFiles: false})
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
