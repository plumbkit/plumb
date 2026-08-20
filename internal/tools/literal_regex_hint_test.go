package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiteralRegexHint_Tiers pins both tiers of the detector. The reported
// friction was that "foo\.bar" — the exact shape an agent writes when it means a
// regex — triggered nothing at all, because the old detector only looked for
// `|`, `.*` and `.+`.
func TestLiteralRegexHint_Tiers(t *testing.T) {
	cases := []struct {
		name        string
		pattern     string
		matched     bool
		wantHint    bool
		wantMention string
	}{
		// Strong tier: flagged whether or not the search matched.
		{"alternation, no match", "a|b", false, true, "| alternation"},
		{"alternation, with matches", "a|b", true, true, "| alternation"},
		{"escaped dot, no match", `foo\.bar`, false, true, `\. escape`},
		{"escaped dot, with matches", `foo\.bar`, true, true, `\. escape`},
		{"digit class", `\d+`, false, true, `\d escape`},
		{"word class", `\wfoo`, true, true, `\w escape`},
		{"dot star", "foo.*bar", true, true, ".* wildcard"},
		{"dot plus", "foo.+bar", true, true, ".* wildcard"},
		{"leading anchor", "^package", true, true, "^ anchor"},

		// Weak tier: flagged ONLY on a zero-match result, because these shapes
		// are ordinary in real source text.
		{"char class, no match", "arr[0]", false, true, "[...] character class"},
		{"char class, with matches", "arr[0]", true, false, ""},
		{"group, no match", "(abc)", false, true, "(...) group"},
		{"group, with matches", "(abc)", true, false, ""},
		{"quantifier, no match", "a{2,4}", false, true, "{n,m} quantifier"},
		{"quantifier, with matches", "a{2,4}", true, false, ""},
		{"trailing anchor, no match", "end$", false, true, "$ anchor"},
		{"trailing anchor, with matches", "end$", true, false, ""},

		// Never flagged in either tier.
		{"plain text", "hello world", false, false, ""},
		{"bare dot", "a.b", false, false, ""},
		{"bare plus", "a+b", false, false, ""},
		{"bare question", "a?", false, false, ""},
		{"string-literal escapes", `line\nnext`, false, false, ""},
		// Boolean-or is ubiquitous in Go/C/JS/shell, and as a regex it means
		// something quite different from what such a search asked for — nudging
		// towards use_regex there is not merely noisy, it is wrong advice.
		{"boolean or, with matches", "err != nil || retry", true, false, ""},
		{"boolean or, no match", "err != nil || retry", false, false, ""},
		{"single bar still flagged", "foo|bar", true, true, "| alternation"},
		{"mixed bars flag the single one", "a||b|c", true, true, "| alternation"},
		{"empty braces", "map[string]struct{}", true, false, ""},
		// An EMPTY group is how ordinary code reads, not how a regex is written;
		// flagging it would send the agent chasing a false lead.
		{"empty group is not regex-shaped", "func main()", false, false, ""},
		{"empty class is not regex-shaped", "x[]", false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := literalRegexHint(tc.pattern, false, tc.matched)
			if tc.wantHint && got == "" {
				t.Fatalf("expected a hint for %q (matched=%v)", tc.pattern, tc.matched)
			}
			if !tc.wantHint && got != "" {
				t.Fatalf("expected NO hint for %q (matched=%v), got: %s", tc.pattern, tc.matched, got)
			}
			if tc.wantMention != "" && !strings.Contains(got, tc.wantMention) {
				t.Errorf("expected hint to name %q, got: %s", tc.wantMention, got)
			}
		})
	}
}

// TestLiteralRegexHint_SilentUnderUseRegex: an explicit regex search is doing
// exactly what it says, so it must never be nudged.
func TestLiteralRegexHint_SilentUnderUseRegex(t *testing.T) {
	for _, matched := range []bool{true, false} {
		if got := literalRegexHint(`a|b\.c`, true, matched); got != "" {
			t.Errorf("use_regex=true must never hint (matched=%v), got: %s", matched, got)
		}
	}
}

// TestSearchInFiles_HintFiresOnNonZeroResult is the behavioural half of the fix:
// the old hint only fired when a search found nothing, so a literal pattern that
// happened to match something was never questioned.
func TestSearchInFiles_HintFiresOnNonZeroResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha|beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{"pattern": "alpha|beta", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.txt") {
		t.Fatalf("expected the literal match, got:\n%s", out)
	}
	if !strings.Contains(out, "use_regex") {
		t.Errorf("expected the hint on a matching literal search, got:\n%s", out)
	}
}

// TestFindReplace_LiteralRegexHint covers the tool that had no nudge at all: a
// zero-change run looked like a confident "nothing to change".
func TestFindReplace_LiteralRegexHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("foo.bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewFindReplace()
	args, _ := json.Marshal(map[string]any{
		"path": dir, "pattern": `foo\.bar`, "replacement": "x", "dry_run": true,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "0 file(s)") {
		t.Fatalf("expected a zero-change run, got:\n%s", out)
	}
	if !strings.Contains(out, "use_regex") {
		t.Errorf("expected a literal-regex hint on the zero-change run, got:\n%s", out)
	}

	// And a plain literal pattern must stay quiet.
	args, _ = json.Marshal(map[string]any{
		"path": dir, "pattern": "foo.bar", "replacement": "x", "dry_run": true,
	})
	out, _ = tool.Execute(context.Background(), args)
	if strings.Contains(out, "use_regex") {
		t.Errorf("a bare dot must not trigger the hint, got:\n%s", out)
	}
}

// TestReadFileSearch_LiteralRegexHint covers read_file's pattern mode, which
// carried a byte-identical copy of the old detector.
func TestReadFileSearch_LiteralRegexHint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewReadFile(nil)
	args, _ := json.Marshal(map[string]any{"file_path": path, "pattern": `alpha\.beta`})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "use_regex") {
		t.Errorf("expected the widened hint for an escaped dot, got:\n%s", out)
	}
}
