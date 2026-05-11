package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupReplaceTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main\n// TODO: fix this\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "b.go"), []byte("package main\n// TODO: also here\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "c.txt"), []byte("nothing to find here\n"), 0o644))
	return dir
}

func TestFindReplace_DryRunDoesNotWrite(t *testing.T) {
	dir := setupReplaceTree(t)
	tool := NewFindReplace()

	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     "TODO",
		"replacement": "DONE",
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("expected DRY RUN marker:\n%s", out)
	}
	// Files should be untouched.
	data, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	if !strings.Contains(string(data), "TODO") {
		t.Error("dry run modified a.go")
	}
}

func TestFindReplace_ApplyChanges(t *testing.T) {
	dir := setupReplaceTree(t)
	tool := NewFindReplace()

	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     "TODO",
		"replacement": "DONE",
		"dry_run":     dryRun,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "DRY RUN") {
		t.Errorf("unexpected DRY RUN marker:\n%s", out)
	}
	for _, name := range []string{"a.go", "b.go"} {
		data, _ := os.ReadFile(filepath.Join(dir, name))
		if strings.Contains(string(data), "TODO") {
			t.Errorf("%s still contains TODO", name)
		}
		if !strings.Contains(string(data), "DONE") {
			t.Errorf("%s missing DONE", name)
		}
	}
}

func TestFindReplace_GlobLimitsScope(t *testing.T) {
	dir := setupReplaceTree(t)
	tool := NewFindReplace()

	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     "TODO",
		"replacement": "DONE",
		"glob":        "a.go",
		"dry_run":     dryRun,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	b, _ := os.ReadFile(filepath.Join(dir, "b.go"))
	if !strings.Contains(string(a), "DONE") {
		t.Error("a.go should be changed")
	}
	if !strings.Contains(string(b), "TODO") {
		t.Error("b.go should NOT be changed")
	}
}

func TestFindReplace_RegexBackrefs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("foo123 bar456\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewFindReplace()

	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     `([a-z]+)(\d+)`,
		"replacement": `$1-$2`,
		"use_regex":   true,
		"dry_run":     dryRun,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "x.txt"))
	if string(data) != "foo-123 bar-456\n" {
		t.Errorf("regex replace wrong: got %q", string(data))
	}
}

func TestFindReplace_InvalidRegex(t *testing.T) {
	tool := NewFindReplace()
	args, _ := json.Marshal(map[string]any{
		"path":        t.TempDir(),
		"pattern":     "[unclosed",
		"replacement": "x",
		"use_regex":   true,
	})
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestFindReplace_SmartCase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("Hello hello HELLO\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewFindReplace()

	// Lowercase pattern → case-insensitive
	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     "hello",
		"replacement": "WORLD",
		"dry_run":     dryRun,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(data) != "WORLD WORLD WORLD\n" {
		t.Errorf("smart-case replace wrong: got %q", string(data))
	}
}
