package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpandBraces(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		want    []string
	}{
		{"no braces returns itself", "*.go", []string{"*.go"}},
		{"empty returns itself", "", []string{""}},
		{"simple alternation", "*.{ts,js}", []string{"*.ts", "*.js"}},
		{"three alternatives", "*.{ts,tsx,js}", []string{"*.ts", "*.tsx", "*.js"}},
		{"prefix and suffix preserved", "src/{a,b}/x.go", []string{"src/a/x.go", "src/b/x.go"}},
		{"empty alternative", "file{,.bak}", []string{"file", "file.bak"}},
		{"nested groups", "{a,b{c,d}}", []string{"a", "bc", "bd"}},
		{"two sibling groups", "{a,b}{c,d}", []string{"ac", "ad", "bc", "bd"}},
		{"doublestar preserved", "**/*.{go,md}", []string{"**/*.go", "**/*.md"}},
		// Shell-fidelity cases: neither of these expands in a real shell either.
		{"group without comma is literal", "{x}", []string{"{x}"}},
		{"unbalanced open brace is literal", "a{b", []string{"a{b"}},
		{"unbalanced close brace is literal", "a}b", []string{"a}b"}},
		{"literal group then real group", "{x}.{go,md}", []string{"{x}.go", "{x}.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandBraces(tc.pattern)
			if err != nil {
				t.Fatalf("expandBraces(%q): %v", tc.pattern, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("expandBraces(%q) = %v, want %v", tc.pattern, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("expandBraces(%q)[%d] = %q, want %q", tc.pattern, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestExpandBraces_Bounds pins that a runaway pattern is REFUSED rather than
// truncated. A silently shortened alternative list would reintroduce exactly the
// silent-wrong-answer failure this change removes.
func TestExpandBraces_Bounds(t *testing.T) {
	// 9 doubling groups = 512 alternatives, past maxBraceExpansions (256).
	if _, err := expandBraces(strings.Repeat("{a,b}", 9)); err == nil {
		t.Error("expected a refusal for a runaway expansion")
	}
	// 8 groups = 256, exactly at the cap, must still be allowed.
	if _, err := expandBraces(strings.Repeat("{a,b}", 8)); err != nil {
		t.Errorf("256 alternatives is at the cap and must be allowed, got: %v", err)
	}
	deep := strings.Repeat("{a,", maxBraceDepth+2) + "z" + strings.Repeat("}", maxBraceDepth+2)
	if _, err := expandBraces(deep); err == nil {
		t.Error("expected a refusal for excessive nesting depth")
	}
}

// TestExpandBraces_GroupCountBound is the regression test for the cost defect an
// independent review found: a long run of comma-less groups ("{x}{x}{x}…")
// hit neither the alternation cap nor the depth cap, because neither counts a
// group that does not alternate. 200k of them took ~20s, re-run per file visited
// during a walk. The group count is now checked up front, in O(n).
func TestExpandBraces_GroupCountBound(t *testing.T) {
	runaway := strings.Repeat("{x}", maxBraceGroups+1)
	start := time.Now()
	_, err := expandBraces(runaway)
	if err == nil {
		t.Fatal("expected a refusal for too many brace groups")
	}
	if !strings.Contains(err.Error(), "brace groups") {
		t.Errorf("expected the group-count message, got: %v", err)
	}
	// The point of counting first is that the refusal is immediate.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("refusal took %s; it should be O(n) and immediate", elapsed)
	}

	// A pathological input that previously took ~20s must now be refused fast.
	huge := strings.Repeat("{x}", 50_000)
	start = time.Now()
	if _, err := expandBraces(huge); err == nil {
		t.Error("expected a refusal for a huge group run")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("huge-input refusal took %s; the count guard did not short-circuit", elapsed)
	}

	// And a normal pattern with a handful of groups still works.
	if got, err := expandBraces("{a,b}/{c,d}.go"); err != nil || len(got) != 4 {
		t.Errorf("ordinary multi-group pattern broke: %v %v", got, err)
	}

	// ESCAPED groups expand to nothing and cost nothing, so they must not count
	// against the cap — counting them would defeat the escape hatch, which exists
	// precisely so a literal-brace pattern behaves as it did before brace support.
	escaped := strings.Repeat(`\{x\}`, maxBraceGroups+8)
	got, err := expandBraces(escaped)
	if err != nil {
		t.Errorf("escaped braces must not count against the group cap, got: %v", err)
	}
	if len(got) != 1 || got[0] != escaped {
		t.Errorf("an all-escaped pattern must pass through unchanged, got %v", got)
	}
}

// TestExpandBraces_EscapedBracesStayLiteral is the regression test for the
// silent behaviour change an independent review found: before brace support,
// `filepath.Match` treated braces as ordinary characters, so a file literally
// named "notes{draft,final}.md" was matchable. Expansion took that away with no
// opt-out — the same silent-wrong-answer class the feature exists to remove.
func TestExpandBraces_EscapedBracesStayLiteral(t *testing.T) {
	got, err := expandBraces(`notes\{draft,final\}.md`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != `notes\{draft,final\}.md` {
		t.Fatalf("escaped braces must not expand, got %v", got)
	}

	// An escaped group next to a real one: only the real one expands.
	got, err = expandBraces(`a\{x,y\}.{go,md}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`a\{x,y\}.go`, `a\{x,y\}.md`}
	if len(got) != len(want) {
		t.Fatalf("expandBraces = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("expandBraces[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestFindFiles_EscapedBraceMatchesLiteralFilename proves the escape works
// end-to-end: filepath.Match unescapes the brace, so the literal file matches.
func TestFindFiles_EscapedBraceMatchesLiteralFilename(t *testing.T) {
	dir := t.TempDir()
	literal := "notes{draft,final}.md"
	if err := os.WriteFile(filepath.Join(dir, literal), []byte("x\n"), 0o644); err != nil {
		t.Skipf("filesystem will not hold a braced filename: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notesdraft.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewFindFiles(nil)
	args, _ := json.Marshal(map[string]any{"pattern": `notes\{draft,final\}.md`, "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("an escaped braced pattern should be supported, got: %v", err)
	}
	if !strings.Contains(out, literal) {
		t.Errorf("expected the literal braced filename to match, got:\n%s", out)
	}
	if strings.Contains(out, "notesdraft.md") {
		t.Errorf("an escaped pattern must not expand into alternatives, got:\n%s", out)
	}
}

// TestFindFiles_BraceGlob is the F2 regression test: "*.{go,md}" returned a
// clean "No files found" because filepath.Match treats the braces as literal
// characters and reports no error for them.
func TestFindFiles_BraceGlob(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.go", "b.md", "c.ts"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewFindFiles(nil)
	args, _ := json.Marshal(map[string]any{"pattern": "*.{go,md}", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("braced pattern should be supported, got: %v", err)
	}
	if strings.Contains(out, "No files found") {
		t.Fatalf("braced pattern silently matched nothing:\n%s", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.md") {
		t.Errorf("expected both alternatives, got:\n%s", out)
	}
	if strings.Contains(out, "c.ts") {
		t.Errorf("an extension outside the braces must not match, got:\n%s", out)
	}
}

// TestFindFiles_BraceGlobWithDirPrefix covers the pruning half of the fix:
// globLiteralPrefix must stop at a brace segment, or the walk prunes away the
// very directories the braced glob was meant to reach.
//
// The file names deliberately do NOT share a substring with the directory names
// in the pattern. An earlier version of this test asserted on "alpha"/"beta",
// which the tool's own no-match message satisfies because it echoes the pattern
// back (`No files found matching "{alpha,beta}/x.go".`) — so it passed with the
// fix reverted, and the pruning half of the fix had no real coverage at all.
func TestFindFiles_BraceGlobWithDirPrefix(t *testing.T) {
	dir := t.TempDir()
	for sub, leaf := range map[string]string{
		"alpha": "first.go", "beta": "second.go", "gamma": "third.go",
	} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, leaf), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewFindFiles(nil)
	args, _ := json.Marshal(map[string]any{"pattern": "{alpha,beta}/*.go", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("braced directory pattern should be supported, got: %v", err)
	}
	if strings.Contains(out, "No files found") {
		t.Fatalf("braced directory pattern matched nothing:\n%s", out)
	}
	// These names appear only in real results, never in the echoed pattern.
	if !strings.Contains(out, "first.go") || !strings.Contains(out, "second.go") {
		t.Errorf("expected both braced directories' files, got:\n%s", out)
	}
	if strings.Contains(out, "third.go") {
		t.Errorf("gamma is outside the braces and must not match, got:\n%s", out)
	}
}

// TestFindReplace_BraceGlob pins the third surface: find_replace's glob silently
// selected no files for a braced pattern, so a replacement reported "0 file(s)"
// and looked like a clean no-op.
func TestFindReplace_BraceGlob(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.ts", "b.tsx", "c.css"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewFindReplace()
	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     "needle",
		"replacement": "thread",
		"glob":        "*.{ts,tsx}",
		"dry_run":     true,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("braced glob should be supported, got: %v", err)
	}
	if !strings.Contains(out, "2 file(s)") {
		t.Errorf("expected both braced alternatives selected, got:\n%s", out)
	}
	if strings.Contains(out, "c.css") {
		t.Errorf("a file outside the braces must not be selected, got:\n%s", out)
	}
}

// TestGitignore_BracesStayLiteral guards the blast radius: .gitignore has no
// brace syntax, so expansion must NOT have leaked into ignore matching. A
// gitignore line "*.{go,md}" ignores only a file literally named that.
func TestGitignore_BracesStayLiteral(t *testing.T) {
	p := ignorePattern{glob: "*.{go,md}"}
	if p.matchesPath("a.go", false) {
		t.Error("gitignore brace pattern must not expand: a.go was ignored")
	}
	if !p.matchesPath("a.{go,md}", false) {
		t.Error("gitignore brace pattern must match the literal name")
	}
}
