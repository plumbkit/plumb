package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchInFiles_Basic(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package main\n\nfunc hello() {}\n")
	write("b.go", "package main\n\nfunc world() {}\n")
	write("c.txt", "hello world\n")

	tool := NewSearchInFiles(nil, nil, nil, 0)

	args, _ := json.Marshal(map[string]any{"pattern": "func hello", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go") {
		t.Errorf("expected a.go in output, got:\n%s", out)
	}
	if strings.Contains(out, "b.go") {
		t.Errorf("b.go should not appear, got:\n%s", out)
	}
}

func TestSearchInFiles_GlobFilter(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "hello from go\n")
	write("readme.txt", "hello from txt\n")

	tool := NewSearchInFiles(nil, nil, nil, 0)

	args, _ := json.Marshal(map[string]any{"pattern": "hello", "path": dir, "glob": "*.go"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("expected main.go, got:\n%s", out)
	}
	if strings.Contains(out, "readme.txt") {
		t.Errorf("readme.txt should be excluded by glob, got:\n%s", out)
	}
}

func TestSearchInFiles_SmartCase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("Hello World\nhello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewSearchInFiles(nil, nil, nil, 0)

	// Lowercase pattern → case-insensitive → both lines match.
	args, _ := json.Marshal(map[string]any{"pattern": "hello", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "> ") < 2 {
		t.Errorf("smart-case: expected 2 match lines, got:\n%s", out)
	}

	// Mixed-case pattern → case-sensitive → only 1 line matches.
	args2, _ := json.Marshal(map[string]any{"pattern": "Hello", "path": dir})
	out2, _ := tool.Execute(context.Background(), args2)
	if strings.Count(out2, "> ") != 1 {
		t.Errorf("case-sensitive: expected 1 match line, got:\n%s", out2)
	}
}

func TestSearchInFiles_ContextLines(t *testing.T) {
	dir := t.TempDir()
	content := "line1\nline2\nTARGET\nline4\nline5\n"
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewSearchInFiles(nil, nil, nil, 0)

	args, _ := json.Marshal(map[string]any{"pattern": "TARGET", "path": dir, "context_lines": 1})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "line2") || !strings.Contains(out, "line4") {
		t.Errorf("context lines missing, got:\n%s", out)
	}
}

func TestSearchInFiles_RespectsGitignore(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vendor", "dep.go"), []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("vendor/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewSearchInFiles(nil, nil, nil, 0)

	args, _ := json.Marshal(map[string]any{"pattern": "needle", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "vendor") {
		t.Errorf("vendor should be gitignored, got:\n%s", out)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("main.go should be found, got:\n%s", out)
	}
}

func TestSearchInFiles_NoMatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewSearchInFiles(nil, nil, nil, 0)

	args, _ := json.Marshal(map[string]any{"pattern": "xyzzy_not_found", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No matches") {
		t.Errorf("expected no-match message, got:\n%s", out)
	}
}

func TestSearchInFiles_InvalidRegex(t *testing.T) {
	dir := t.TempDir()
	tool := NewSearchInFiles(nil, nil, nil, 0)

	// An invalid regex is only rejected when use_regex:true.
	// With the default literal mode, "[invalid" is a perfectly valid literal string.
	args, _ := json.Marshal(map[string]any{"pattern": "[invalid", "path": dir, "use_regex": true})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("expected error for invalid regex with use_regex:true")
	}
}

// TestSearchInFiles_LiteralMode verifies that patterns containing regex
// metacharacters are treated as plain text when use_regex is false (the
// default). A literal search for "a.b" must not match "acb" the way a regex
// would.
func TestSearchInFiles_LiteralMode(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// File with the literal string; file with a regex-wildcard expansion that
	// should NOT match in literal mode.
	write("match.txt", "foo.bar and more\n")
	write("nomatch.txt", "fooXbar\n")

	tool := NewSearchInFiles(nil, nil, nil, 0)

	// Default: no use_regex field → literal.
	for _, tc := range []struct {
		pattern string
		want    string
		nowant  string
	}{
		{"foo.bar", "match.txt", "nomatch.txt"},
		{"plumb.daemon.lock", "", ""},
		{"foo(x)", "", ""},
		{"[bracket", "", ""},
	} {
		args, _ := json.Marshal(map[string]any{"pattern": tc.pattern, "path": dir})
		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Errorf("pattern %q: unexpected error: %v", tc.pattern, err)
			continue
		}
		if tc.want != "" && !strings.Contains(out, tc.want) {
			t.Errorf("pattern %q: expected %q in output:\n%s", tc.pattern, tc.want, out)
		}
		if tc.nowant != "" && strings.Contains(out, tc.nowant) {
			t.Errorf("pattern %q: %q should not appear in output:\n%s", tc.pattern, tc.nowant, out)
		}
	}

	// Explicit use_regex:false also yields literal behaviour.
	args, _ := json.Marshal(map[string]any{"pattern": "foo.bar", "path": dir, "use_regex": false})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("use_regex:false: unexpected error: %v", err)
	}
	if !strings.Contains(out, "match.txt") {
		t.Errorf("use_regex:false: expected match.txt:\n%s", out)
	}
	if strings.Contains(out, "nomatch.txt") {
		t.Errorf("use_regex:false: nomatch.txt should not appear:\n%s", out)
	}
}

// TestSearchInFiles_UseRegex verifies that use_regex:true restores regex
// semantics, including wildcard expansions and anchors.
func TestSearchInFiles_UseRegex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("foobar\nfoo123bar\nbaz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewSearchInFiles(nil, nil, nil, 0)

	args, _ := json.Marshal(map[string]any{"pattern": "foo.*bar", "path": dir, "use_regex": true})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Both foobar and foo123bar match the regex; baz does not.
	if strings.Count(out, "> ") < 2 {
		t.Errorf("expected 2 regex matches (foobar + foo123bar), got:\n%s", out)
	}
	if strings.Contains(out, "baz") {
		t.Errorf("baz should not match foo.*bar:\n%s", out)
	}
}

// TestSearchInFiles_MaxFileBytesSkipsLargeFiles verifies that files larger
// than max_file_bytes are skipped before opening — their content is never
// scanned, so matches inside them don't show up.
func TestSearchInFiles_MaxFileBytesSkipsLargeFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("needle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 0, 64*1024)
	for len(big) < cap(big) {
		big = append(big, []byte("needle and more text\n")...)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{
		"pattern":        "needle",
		"path":           dir,
		"max_file_bytes": 1024, // big.txt is ~64 KiB
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "small.txt") {
		t.Errorf("expected small.txt in output:\n%s", out)
	}
	if strings.Contains(out, "big.txt") {
		t.Errorf("big.txt should have been skipped (exceeds max_file_bytes):\n%s", out)
	}
}

func TestSearchInFiles_MatchAfterOversizedLine(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", searchMaxLineBytes+1)
	content := long + "\nneedle after long line\n"
	if err := os.WriteFile(filepath.Join(dir, "generated.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{
		"pattern": "needle",
		"path":    dir,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "generated.txt") || !strings.Contains(out, "2:> needle after long line") {
		t.Fatalf("expected match after oversized line with original line number:\n%s", out)
	}
	if !strings.Contains(out, "oversized line(s) skipped") {
		t.Fatalf("expected oversized-line warning:\n%s", out)
	}
}

// TestSearchInFiles_ManyFiles_ParallelCorrectness writes many matching files
// and verifies the parallel scan returns every file's content and the output
// is sorted by relative path.
func TestSearchInFiles_ManyFiles_ParallelCorrectness(t *testing.T) {
	dir := t.TempDir()
	const n = 200
	for i := range n {
		path := filepath.Join(dir, fmt.Sprintf("f%03d.go", i))
		if err := os.WriteFile(path, []byte("package main\n// MARKER\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{
		"pattern":     "MARKER",
		"path":        dir,
		"max_results": n + 10, // allow all hits without truncation
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, fmt.Sprintf("%d hit(s) across %d file(s)", n, n)) {
		t.Fatalf("expected %d hits / %d files summary:\n%s", n, n, out)
	}
	// Output should be sorted by path: f000.go before f001.go before ... .
	idx0 := strings.Index(out, "f000.go")
	idx199 := strings.Index(out, "f199.go")
	if idx0 < 0 || idx199 < 0 || idx0 >= idx199 {
		t.Errorf("output not sorted by path (f000 at %d, f199 at %d)", idx0, idx199)
	}
}

// TestSearchInFiles_ExcludeSuppresesMatch verifies that an exclude pattern hides
// matches that would otherwise appear.
func TestSearchInFiles_ExcludeSuppresesMatch(t *testing.T) {
	dir := t.TempDir()
	mustMkdir := func(p string) {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(p, content string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir(filepath.Join(dir, "vendor", "dep"))
	mustWrite(filepath.Join(dir, "main.go"), "needle\n")
	mustWrite(filepath.Join(dir, "vendor", "dep", "lib.go"), "needle in vendor\n")

	tool := NewSearchInFiles(nil, nil, nil, 0)

	// Without exclude: both files match.
	args, _ := json.Marshal(map[string]any{"pattern": "needle", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go") || !strings.Contains(out, "vendor") {
		t.Errorf("expected both matches without exclude:\n%s", out)
	}

	// With exclude: vendor subtree is pruned.
	args2, _ := json.Marshal(map[string]any{
		"pattern": "needle",
		"path":    dir,
		"exclude": []string{"vendor"},
	})
	out2, err := tool.Execute(context.Background(), args2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "main.go") {
		t.Errorf("main.go should still match:\n%s", out2)
	}
	if strings.Contains(out2, "vendor") {
		t.Errorf("vendor/ should be excluded:\n%s", out2)
	}
}

// TestSearchInFiles_ExcludeByGlob verifies that glob patterns in exclude work
// against file base names.
func TestSearchInFiles_ExcludeByGlob(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "needle\n")
	write("main.pb.go", "needle in generated\n")

	tool := NewSearchInFiles(nil, nil, nil, 0)

	args, _ := json.Marshal(map[string]any{
		"pattern": "needle",
		"path":    dir,
		"exclude": []string{"*.pb.go"},
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("main.go should match:\n%s", out)
	}
	if strings.Contains(out, "main.pb.go") {
		t.Errorf("main.pb.go should be excluded by glob:\n%s", out)
	}
}

// TestSearchInFiles_GlobPrunesSiblingDirs verifies that a glob with a literal
// directory prefix prunes sibling subtrees so matches inside them never
// surface.
func TestSearchInFiles_GlobPrunesSiblingDirs(t *testing.T) {
	dir := t.TempDir()
	mustMkdir := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(p, content string) {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir(filepath.Join(dir, "wanted", "deep"))
	mustMkdir(filepath.Join(dir, "skipme", "deep"))
	mustWrite(filepath.Join(dir, "wanted", "a.txt"), "MATCH here\n")
	mustWrite(filepath.Join(dir, "wanted", "deep", "b.txt"), "MATCH here\n")
	mustWrite(filepath.Join(dir, "skipme", "c.txt"), "MATCH but pruned\n")
	mustWrite(filepath.Join(dir, "skipme", "deep", "d.txt"), "MATCH but pruned\n")

	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{
		"pattern": "MATCH",
		"path":    dir,
		"glob":    "wanted/**/*.txt",
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wanted/a.txt") || !strings.Contains(out, "wanted/deep/b.txt") {
		t.Errorf("expected both wanted/ matches:\n%s", out)
	}
	if strings.Contains(out, "skipme/") {
		t.Errorf("skipme/ subtree should have been pruned:\n%s", out)
	}
}

// BenchmarkSearchInFiles_WarmCall measures search latency with a pre-existing
// directory tree that fits in the OS page cache. It proxies for the common
// case of repeated searches in the same workspace and is a lower bound on the
// p95 latency target (< 200 ms for a typical project).
func BenchmarkSearchInFiles_WarmCall(b *testing.B) {
	dir := b.TempDir()
	mustWrite := func(path, content string) {
		b.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	// Write enough files that the directory walk is non-trivial.
	for i := range 50 {
		mustWrite(filepath.Join(dir, fmt.Sprintf("sub%d", i/10), fmt.Sprintf("file%d.go", i)),
			fmt.Sprintf("package p\n\nfunc F%d() {}\n// target marker\n", i))
	}

	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{"pattern": "target marker", "path": dir})
	ctx := context.Background()

	// Warm run: execute once outside the loop so the OS cache is warm.
	if _, err := tool.Execute(ctx, args); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		if _, err := tool.Execute(ctx, args); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSearchInFiles_BraceGlobReturnsError(t *testing.T) {
	dir := t.TempDir()
	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{
		"pattern": "hello",
		"path":    dir,
		"glob":    "*.{go,ts}",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "brace alternation") {
		t.Fatalf("expected brace-alternation error, got: %v", err)
	}
}

func TestSearchInFiles_FilePathSearchesDirectoryWithNote(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.go"), []byte("needle\n"), 0o644)
	targetFile := filepath.Join(dir, "a.go")
	tool := NewSearchInFiles(nil, nil, nil, 0)
	args, _ := json.Marshal(map[string]any{
		"pattern": "needle",
		"path":    targetFile,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Both files found because the whole directory was searched.
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Errorf("expected both files when path is a file; got:\n%s", out)
	}
	// A redirect note must be present.
	if !strings.Contains(out, "Note:") || !strings.Contains(out, "containing directory") {
		t.Errorf("expected path-redirect note in output; got:\n%s", out)
	}
}
