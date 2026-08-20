package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
func TestFindFiles_BraceGlobWithDirPrefix(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"alpha", "beta", "gamma"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, "x.go"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewFindFiles(nil)
	args, _ := json.Marshal(map[string]any{"pattern": "{alpha,beta}/x.go", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("braced directory pattern should be supported, got: %v", err)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Errorf("expected both braced directories, got:\n%s", out)
	}
	if strings.Contains(out, "gamma") {
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
