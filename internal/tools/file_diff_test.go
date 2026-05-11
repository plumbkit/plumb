package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func requireDiff(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("diff"); err != nil {
		t.Skip("diff not on PATH")
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "plumb-diff-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return f.Name()
}

func TestFileDiff_IdenticalFiles(t *testing.T) {
	requireDiff(t)
	a := writeTempFile(t, "hello\nworld\n")
	b := writeTempFile(t, "hello\nworld\n")

	tool := NewFileDiff()
	raw, _ := json.Marshal(map[string]any{"file_a": a, "file_b": b})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("file_diff: %v", err)
	}
	if out != "(files are identical)" {
		t.Fatalf("expected identical message, got: %q", out)
	}
}

func TestFileDiff_DifferentFiles(t *testing.T) {
	requireDiff(t)
	a := writeTempFile(t, "hello\nworld\n")
	b := writeTempFile(t, "hello\nearth\n")

	tool := NewFileDiff()
	raw, _ := json.Marshal(map[string]any{"file_a": a, "file_b": b})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("file_diff: %v", err)
	}
	if !strings.Contains(out, "-world") || !strings.Contains(out, "+earth") {
		t.Fatalf("expected unified diff with changes, got: %q", out)
	}
}

func TestFileDiff_ContextLines(t *testing.T) {
	requireDiff(t)
	content := "line1\nline2\nline3\nCHANGED\nline5\nline6\nline7\n"
	a := writeTempFile(t, content)
	b := writeTempFile(t, strings.Replace(content, "CHANGED", "altered", 1))

	tool := NewFileDiff()
	n := 1
	raw, _ := json.Marshal(map[string]any{"file_a": a, "file_b": b, "context_lines": &n})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("file_diff: %v", err)
	}
	// With 1 line of context, line1 and line7 should not appear.
	if strings.Contains(out, "line1") || strings.Contains(out, "line7") {
		t.Fatalf("expected only 1 line of context, got:\n%s", out)
	}
}

func TestFileDiff_IgnoreWhitespace(t *testing.T) {
	requireDiff(t)
	a := writeTempFile(t, "hello world\n")
	b := writeTempFile(t, "hello  world\n") // extra space

	tool := NewFileDiff()
	raw, _ := json.Marshal(map[string]any{
		"file_a":            a,
		"file_b":            b,
		"ignore_whitespace": true,
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("file_diff: %v", err)
	}
	if out != "(files are identical)" {
		t.Fatalf("expected identical with whitespace ignored, got: %q", out)
	}
}

func TestFileDiff_MissingFile(t *testing.T) {
	requireDiff(t)
	tool := NewFileDiff()
	raw, _ := json.Marshal(map[string]any{
		"file_a": "/nonexistent/path/a.txt",
		"file_b": "/nonexistent/path/b.txt",
	})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for missing files")
	}
}

func TestFileDiff_MissingArgs(t *testing.T) {
	requireDiff(t)
	tool := NewFileDiff()
	raw, _ := json.Marshal(map[string]any{"file_a": "/tmp/a.txt"})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error when file_b is missing")
	}
}
