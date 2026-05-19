package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupFindTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "internal", "tools"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "vendor", "lib"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "internal", "tools", "foo_test.go"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "internal", "tools", "bar.go"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "vendor", "lib", "dep.go"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("vendor/\n"), 0o644))
	return dir
}

func TestFindFiles_GlobPattern(t *testing.T) {
	dir := setupFindTree(t)
	tool := NewFindFiles(nil)

	args, _ := json.Marshal(map[string]any{"pattern": "*_test.go", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "foo_test.go") {
		t.Errorf("expected foo_test.go, got:\n%s", out)
	}
	if strings.Contains(out, "bar.go") {
		t.Errorf("bar.go should not match *_test.go, got:\n%s", out)
	}
}

func TestFindFiles_RespectsGitignore(t *testing.T) {
	dir := setupFindTree(t)
	tool := NewFindFiles(nil)

	args, _ := json.Marshal(map[string]any{"pattern": "*.go", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "vendor") {
		t.Errorf("vendor/ should be gitignored, got:\n%s", out)
	}
}

func TestFindFiles_RegexMode(t *testing.T) {
	dir := setupFindTree(t)
	tool := NewFindFiles(nil)

	args, _ := json.Marshal(map[string]any{"pattern": `.*_test\.go$`, "path": dir, "use_regex": true})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "foo_test.go") {
		t.Errorf("expected foo_test.go in regex match, got:\n%s", out)
	}
}

func TestFindFiles_ExtensionFilter(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "b.ts"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "c.go"), []byte("x"), 0o644))

	tool := NewFindFiles(nil)
	args, _ := json.Marshal(map[string]any{"pattern": "*", "path": dir, "extension": "go"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "b.ts") {
		t.Errorf("b.ts should be excluded by extension filter, got:\n%s", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "c.go") {
		t.Errorf("expected a.go and c.go, got:\n%s", out)
	}
}

func TestFindFiles_TypeDir(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "mydir"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "myfile"), []byte("x"), 0o644))

	tool := NewFindFiles(nil)
	args, _ := json.Marshal(map[string]any{"pattern": "*", "path": dir, "type": "dir"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "myfile") {
		t.Errorf("myfile should not appear for type=dir, got:\n%s", out)
	}
	if !strings.Contains(out, "mydir") {
		t.Errorf("mydir should appear for type=dir, got:\n%s", out)
	}
}

func TestFindFiles_NoMatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewFindFiles(nil)

	args, _ := json.Marshal(map[string]any{"pattern": "*.rs", "path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No files found") {
		t.Errorf("expected no-match message, got:\n%s", out)
	}
}

func TestFindFiles_CancelledContextReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewFindFiles(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	args, _ := json.Marshal(map[string]any{"pattern": "*.go", "path": dir})
	if _, err := tool.Execute(ctx, args); err == nil {
		t.Fatal("expected cancellation error")
	}
}

// TestFindFiles_GlobPrunesSiblingDirs verifies that a glob with a literal
// path prefix (e.g. "wanted/**") never returns hits from sibling subtrees,
// even when files inside those subtrees would have matched the trailing glob
// portion. The walk should not descend into pruned subtrees at all.
func TestFindFiles_GlobPrunesSiblingDirs(t *testing.T) {
	dir := t.TempDir()
	mustMkdir := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(p string) {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir(filepath.Join(dir, "wanted", "deep"))
	mustMkdir(filepath.Join(dir, "skipme", "deep"))
	mustWrite(filepath.Join(dir, "wanted", "a.go"))
	mustWrite(filepath.Join(dir, "wanted", "deep", "b.go"))
	mustWrite(filepath.Join(dir, "skipme", "c.go"))
	mustWrite(filepath.Join(dir, "skipme", "deep", "d.go"))

	tool := NewFindFiles(nil)
	args, _ := json.Marshal(map[string]any{
		"pattern": "wanted/**/*.go",
		"path":    dir,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wanted/a.go") || !strings.Contains(out, "wanted/deep/b.go") {
		t.Errorf("expected both wanted/ matches:\n%s", out)
	}
	if strings.Contains(out, "skipme/") {
		t.Errorf("skipme/ subtree should have been pruned:\n%s", out)
	}
}
