package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// TestFindReplace_ManyFiles_ParallelCorrectness writes many matching files and
// verifies every one is replaced exactly once under the parallel worker pool.
func TestFindReplace_ManyFiles_ParallelCorrectness(t *testing.T) {
	dir := t.TempDir()
	const n = 200
	for i := 0; i < n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
		if err := os.WriteFile(path, []byte("alpha alpha alpha\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := NewFindReplace()
	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     "alpha",
		"replacement": "BETA",
		"dry_run":     dryRun,
		"max_files":   n + 10,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	expectedHeader := fmt.Sprintf("%d file(s), %d replacement(s) changed", n, n*3)
	if !strings.Contains(out, expectedHeader) {
		t.Errorf("expected header %q in output:\n%s", expectedHeader, out)
	}

	for i := 0; i < n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
		data, _ := os.ReadFile(path)
		if string(data) != "BETA BETA BETA\n" {
			t.Errorf("%s = %q, want BETA x3", filepath.Base(path), string(data))
		}
	}
}

// TestFindReplace_MaxFilesIsExact verifies that with max_files=N and >N
// matching files, exactly N files are written (no over-writing past the cap
// due to parallelism).
func TestFindReplace_MaxFilesIsExact(t *testing.T) {
	dir := t.TempDir()
	const total = 60
	const maxN = 10
	for i := 0; i < total; i++ {
		path := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
		if err := os.WriteFile(path, []byte("xx\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := NewFindReplace()
	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     "xx",
		"replacement": "yy",
		"dry_run":     dryRun,
		"max_files":   maxN,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	written := 0
	for i := 0; i < total; i++ {
		data, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)))
		switch string(data) {
		case "yy\n":
			written++
		case "xx\n":
			// untouched
		default:
			t.Errorf("unexpected content %q in f%03d.txt", string(data), i)
		}
	}
	if written != maxN {
		t.Errorf("max_files cap not exact: wrote %d files, want %d", written, maxN)
	}
}

// TestFindReplace_BinaryFilesSkipped verifies the sniff-first path: a large
// file whose first 8 KB contains a null byte must not be scanned beyond the
// sniff, and must not be modified.
func TestFindReplace_BinaryFilesSkipped(t *testing.T) {
	dir := t.TempDir()
	// Text file that should be modified.
	textPath := filepath.Join(dir, "text.txt")
	if err := os.WriteFile(textPath, []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Binary file: null byte within the sniff window, then "needle" matches
	// well past the sniff window (>8 KB) that must NOT be touched.
	binPath := filepath.Join(dir, "blob.bin")
	binData := make([]byte, 32*1024)
	copy(binData, "\x00\x00\x00leading null bytes\n")
	for i := 100; i+6 < len(binData); i += 7 {
		copy(binData[i:], "needle ")
	}
	if err := os.WriteFile(binPath, binData, 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewFindReplace()
	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     "needle",
		"replacement": "HAY",
		"dry_run":     dryRun,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 file(s), 1 replacement(s) changed") {
		t.Errorf("expected exactly 1 file changed (text only):\n%s", out)
	}

	got, _ := os.ReadFile(textPath)
	if string(got) != "HAY\n" {
		t.Errorf("text file not modified: %q", string(got))
	}
	binGot, _ := os.ReadFile(binPath)
	if !bytes.Equal(binGot, binData) {
		t.Error("binary file was modified or truncated")
	}
}

// TestFindReplace_OutputSortedByPath checks that the result list is stable
// (sorted by path) regardless of worker scheduling order.
func TestFindReplace_OutputSortedByPath(t *testing.T) {
	dir := t.TempDir()
	names := []string{"zzz.txt", "aaa.txt", "mmm.txt", "bbb.txt"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("hit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := NewFindReplace()
	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     "hit",
		"replacement": "HIT",
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	// Extract the order in which paths appear in the output.
	lines := strings.Split(out, "\n")
	var order []string
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		for _, name := range names {
			if strings.HasPrefix(ln, name) {
				order = append(order, name)
			}
		}
	}
	want := []string{"aaa.txt", "bbb.txt", "mmm.txt", "zzz.txt"}
	if len(order) != len(want) {
		t.Fatalf("got %d names in output, want %d:\n%s", len(order), len(want), out)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("output order = %v, want %v", order, want)
			break
		}
	}
}

// TestFindReplace_ContextCancellation verifies that cancelling the parent
// context stops further file processing rather than draining the entire tree.
func TestFindReplace_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 50; i++ {
		path := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
		if err := os.WriteFile(path, []byte("kw\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := NewFindReplace()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before starting

	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     "kw",
		"replacement": "KW",
		"dry_run":     false,
	})
	if _, err := tool.Execute(ctx, args); err != nil {
		// An error from cancellation is acceptable; what we really care about
		// is that we don't deadlock and that we don't write every file.
		t.Logf("Execute returned err on cancelled ctx: %v", err)
	}

	written := 0
	for i := 0; i < 50; i++ {
		data, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)))
		if string(data) == "KW\n" {
			written++
		}
	}
	if written == 50 {
		t.Errorf("ctx cancellation didn't stop processing: wrote all %d files", written)
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
