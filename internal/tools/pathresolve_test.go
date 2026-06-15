package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wsFn returns a WorkspaceFn that always resolves to root.
func wsFn(root string) WorkspaceFn { return func() string { return root } }

func TestResolvePath(t *testing.T) {
	ws := "/work/space"
	tests := []struct {
		name string
		in   string
		ws   WorkspaceFn
		want string
	}{
		{"absolute unchanged", "/abs/x.go", wsFn(ws), "/abs/x.go"},
		{"file uri stripped, absolute", "file:///abs/x.go", wsFn(ws), "/abs/x.go"},
		{"relative anchored to workspace", "app/x.go", wsFn(ws), filepath.Join(ws, "app/x.go")},
		{"file uri relative anchored", "file://app/x.go", wsFn(ws), filepath.Join(ws, "app/x.go")},
		{"relative with nil ws stays relative", "app/x.go", nil, "app/x.go"},
		{"relative with empty ws stays relative", "app/x.go", wsFn(""), "app/x.go"},
		{"escaping relative is anchored then cleaned", "../other/x.go", wsFn(ws), filepath.Join(ws, "../other/x.go")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePath(tc.in, tc.ws); got != tc.want {
				t.Errorf("resolvePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWriteDepsResolvePath(t *testing.T) {
	ws := "/work/space"
	tests := []struct {
		name string
		in   string
		deps WriteDeps
		want string
	}{
		{"absolute unchanged", "/abs/x.go", WriteDeps{WorkspaceFn: wsFn(ws)}, "/abs/x.go"},
		{"relative anchored", "app/x.go", WriteDeps{WorkspaceFn: wsFn(ws)}, filepath.Join(ws, "app/x.go")},
		{"file uri relative anchored", "file://app/x.go", WriteDeps{WorkspaceFn: wsFn(ws)}, filepath.Join(ws, "app/x.go")},
		{"relative with nil WorkspaceFn stays relative", "app/x.go", WriteDeps{}, "app/x.go"},
		{"relative with empty workspace stays relative", "app/x.go", WriteDeps{WorkspaceFn: wsFn("")}, "app/x.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.deps.resolvePath(tc.in); got != tc.want {
				t.Errorf("WriteDeps.resolvePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestToFileURIAnchored(t *testing.T) {
	ws := "/work/space"
	tests := []struct {
		name string
		in   string
		ws   WorkspaceFn
		want string
	}{
		{"empty stays empty", "", wsFn(ws), ""},
		{"file uri unchanged", "file:///abs/x.go", wsFn(ws), "file:///abs/x.go"},
		{"absolute gains scheme", "/abs/x.go", wsFn(ws), "file:///abs/x.go"},
		{"relative anchored then schemed", "app/x.go", wsFn(ws), "file://" + filepath.Join(ws, "app/x.go")},
		{"relative with nil ws left relative", "app/x.go", nil, "file://app/x.go"},
		{"relative with empty ws left relative", "app/x.go", wsFn(""), "file://app/x.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toFileURIAnchored(tc.in, tc.ws); got != tc.want {
				t.Errorf("toFileURIAnchored(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWriteFileResolvesRelativePath(t *testing.T) {
	ws := t.TempDir()
	deps := WriteDeps{Boundary: testBoundaryGuard(ws), WorkspaceFn: wsFn(ws)}
	_, err := NewWriteFile(deps).Execute(context.Background(), mustBoundaryJSON(t, map[string]string{
		"file_path": "sub/new.txt",
		"content":   "hi",
	}))
	if err != nil {
		t.Fatalf("write_file with relative path failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, "sub", "new.txt"))
	if err != nil {
		t.Fatalf("expected file at workspace-relative path: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("content = %q, want %q", got, "hi")
	}
}

func TestWriteFileRelativeEscapeRejected(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "workspace")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{Boundary: testBoundaryGuard(ws), WorkspaceFn: wsFn(ws)}
	_, err := NewWriteFile(deps).Execute(context.Background(), mustBoundaryJSON(t, map[string]string{
		"file_path": "../escape.txt",
		"content":   "x",
	}))
	if err == nil || !strings.Contains(err.Error(), "workspace boundary violation") {
		t.Fatalf("expected boundary violation for escaping relative path, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(base, "escape.txt")); statErr == nil {
		t.Fatal("escaping relative path wrote outside the workspace")
	}
}

func TestReadFileResolvesRelativePath(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "foo.txt"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewReadFile(nil).WithBoundary(testBoundaryGuard(ws)).WithWorkspace(wsFn(ws))
	out, err := tool.Execute(context.Background(), mustBoundaryJSON(t, map[string]string{"file_path": "foo.txt"}))
	if err != nil {
		t.Fatalf("read_file with relative path failed: %v", err)
	}
	if !strings.Contains(out, "body") {
		t.Fatalf("read_file output missing content: %q", out)
	}
}

func TestEditFileResolvesRelativePath(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "foo.txt"), []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{Boundary: testBoundaryGuard(ws), WorkspaceFn: wsFn(ws)}
	_, err := NewEditFile(deps).Execute(context.Background(), mustBoundaryJSON(t, map[string]any{
		"file_path": "foo.txt",
		"edits":     []map[string]string{{"old_string": "before", "new_string": "after"}},
	}))
	if err != nil {
		t.Fatalf("edit_file with relative path failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, "foo.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after" {
		t.Fatalf("content = %q, want %q", got, "after")
	}
}

func TestRenameFileResolvesRelativePaths(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "src.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{Boundary: testBoundaryGuard(ws), WorkspaceFn: wsFn(ws)}
	_, err := NewRenameFile(deps).Execute(context.Background(), mustBoundaryJSON(t, map[string]string{
		"from": "src.txt",
		"to":   "sub/dst.txt",
	}))
	if err != nil {
		t.Fatalf("rename_file with relative paths failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "sub", "dst.txt")); err != nil {
		t.Fatalf("destination not at workspace-relative path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "src.txt")); !os.IsNotExist(err) {
		t.Fatalf("source still present after rename")
	}
}

// TestRenameFileSamePathAfterResolution guards that the from==to check runs on
// the resolved paths, so a relative and absolute spelling of one file collide.
func TestRenameFileSamePathAfterResolution(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "foo.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{Boundary: testBoundaryGuard(ws), WorkspaceFn: wsFn(ws)}
	_, err := NewRenameFile(deps).Execute(context.Background(), mustBoundaryJSON(t, map[string]string{
		"from": "foo.txt",
		"to":   filepath.Join(ws, "foo.txt"),
	}))
	if err == nil || !strings.Contains(err.Error(), "same path") {
		t.Fatalf("expected same-path rejection after resolution, got %v", err)
	}
}

func TestCopyFileResolvesRelativePaths(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "src.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{Boundary: testBoundaryGuard(ws), WorkspaceFn: wsFn(ws)}
	_, err := NewCopyFile(deps).Execute(context.Background(), mustBoundaryJSON(t, map[string]string{
		"from": "src.txt",
		"to":   "sub/copy.txt",
	}))
	if err != nil {
		t.Fatalf("copy_file with relative paths failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, "sub", "copy.txt"))
	if err != nil {
		t.Fatalf("copy not at workspace-relative path: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("content = %q, want %q", got, "data")
	}
}

func TestFindReplaceResolvesRelativePath(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "foo.txt"), []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{Boundary: testBoundaryGuard(ws), WorkspaceFn: wsFn(ws)}
	_, err := NewFindReplace(deps).Execute(context.Background(), mustBoundaryJSON(t, map[string]any{
		"path":        "foo.txt",
		"pattern":     "before",
		"replacement": "after",
		"dry_run":     false,
	}))
	if err != nil {
		t.Fatalf("find_replace with relative path failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, "foo.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after" {
		t.Fatalf("content = %q, want %q", got, "after")
	}
}

func TestTransactionApplyResolvesRelativePaths(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{Boundary: testBoundaryGuard(ws), WorkspaceFn: wsFn(ws), Writes: NewWriteTracker()}
	_, err := NewTransactionApply(deps).Execute(context.Background(), mustBoundaryJSON(t, map[string]any{
		"operations": []map[string]any{
			{"file_path": "a.txt", "edits": []map[string]string{{"old_string": "one", "new_string": "1"}}},
			{"file_path": "b.txt", "edits": []map[string]string{{"old_string": "two", "new_string": "2"}}},
		},
	}))
	if err != nil {
		t.Fatalf("transaction_apply with relative paths failed: %v", err)
	}
	for name, want := range map[string]string{"a.txt": "1", "b.txt": "2"} {
		got, rErr := os.ReadFile(filepath.Join(ws, name))
		if rErr != nil {
			t.Fatal(rErr)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}
