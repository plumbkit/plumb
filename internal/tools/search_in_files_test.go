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

	tool := NewSearchInFiles()

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

	tool := NewSearchInFiles()

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
	tool := NewSearchInFiles()

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
	tool := NewSearchInFiles()

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
	tool := NewSearchInFiles()

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
	tool := NewSearchInFiles()

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
	tool := NewSearchInFiles()

	args, _ := json.Marshal(map[string]any{"pattern": "[invalid", "path": dir})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("expected error for invalid regex")
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

	tool := NewSearchInFiles()
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
	tool := NewSearchInFiles()
	args, _ := json.Marshal(map[string]any{
		"pattern":     "MARKER",
		"path":        dir,
		"max_results": n + 10, // allow all hits without truncation
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, fmt.Sprintf("%d file(s) matched, %d hits", n, n)) {
		t.Fatalf("expected %d files / %d hits summary:\n%s", n, n, out)
	}
	// Output should be sorted by path: f000.go before f001.go before ... .
	idx0 := strings.Index(out, "f000.go")
	idx199 := strings.Index(out, "f199.go")
	if idx0 < 0 || idx199 < 0 || idx0 >= idx199 {
		t.Errorf("output not sorted by path (f000 at %d, f199 at %d)", idx0, idx199)
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

	tool := NewSearchInFiles()
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
