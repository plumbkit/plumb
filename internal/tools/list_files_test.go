package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/tools"
)

func makeTestTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		"main.go",
		"main_test.go",
		"README.md",
		".hidden",
		"sub/util.go",
		"sub/util_test.go",
		"vendor/dep.go",
		".git/config",
	}
	for _, f := range files {
		full := filepath.Join(root, f)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, nil, 0o644)
	}
	return root
}

func TestListFiles_AllFiles(t *testing.T) {
	root := makeTestTree(t)
	tool := tools.NewListFiles(nil)
	raw, _ := json.Marshal(map[string]any{"root": root})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("list_files: %v", err)
	}
	// vendor and .git are always excluded; .hidden is excluded by default
	for _, unwanted := range []string{"vendor", ".git", ".hidden"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output should not contain %q:\n%s", unwanted, out)
		}
	}
	for _, wanted := range []string{"main.go", "main_test.go", "README.md", "sub/util.go"} {
		if !strings.Contains(out, wanted) {
			t.Errorf("output missing %q:\n%s", wanted, out)
		}
	}
}

func TestListFiles_GoOnly(t *testing.T) {
	root := makeTestTree(t)
	tool := tools.NewListFiles(nil)
	raw, _ := json.Marshal(map[string]any{"root": root, "pattern": "*.go"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("list_files: %v", err)
	}
	if strings.Contains(out, "README.md") {
		t.Errorf("README.md should not appear with *.go filter:\n%s", out)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("main.go missing:\n%s", out)
	}
}

func TestListFiles_MaxDepth(t *testing.T) {
	root := makeTestTree(t)
	tool := tools.NewListFiles(nil)
	depth := 1
	raw, _ := json.Marshal(map[string]any{"root": root, "max_depth": &depth})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("list_files: %v", err)
	}
	if strings.Contains(out, "sub") {
		t.Errorf("sub/ should not appear at depth 1:\n%s", out)
	}
}

func TestListFiles_IncludeHidden(t *testing.T) {
	root := makeTestTree(t)
	tool := tools.NewListFiles(nil)
	raw, _ := json.Marshal(map[string]any{"root": root, "include_hidden": true})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("list_files: %v", err)
	}
	if !strings.Contains(out, ".hidden") {
		t.Errorf("expected .hidden when include_hidden=true:\n%s", out)
	}
}

func TestListFiles_DefaultRoot(t *testing.T) {
	tool := tools.NewListFiles(nil)
	raw, _ := json.Marshal(map[string]any{"pattern": "*.go"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("list_files with default root: %v", err)
	}
	// Should find at least this test file.
	if !strings.Contains(out, ".go") {
		t.Errorf("expected Go files from cwd:\n%s", out)
	}
}
