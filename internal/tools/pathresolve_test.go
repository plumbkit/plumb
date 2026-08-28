package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
)

// wsFn returns a WorkspaceFn that always resolves to root.
func wsFn(root string) WorkspaceFn { return func(context.Context) string { return root } }

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
			if got, err := resolvePath(context.Background(), tc.in, tc.ws, nil); err != nil {
				t.Errorf("resolvePath(%q) = error %v", tc.in, err)
			} else if got != tc.want {
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
			if got, err := tc.deps.resolvePath(context.Background(), tc.in); err != nil {
				t.Errorf("WriteDeps.resolvePath(%q) = error %v", tc.in, err)
			} else if got != tc.want {
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
			if got := toFileURIAnchored(context.Background(), tc.in, tc.ws); got != tc.want {
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

// TestRenameFileTwoSpellingsOfOneFileRefused is the deadlock regression: from
// and to are two spellings of ONE file (a symlinked parent). With the
// same-place check comparing raw strings and the lock keyed canonically, the
// call took one non-reentrant mutex twice and blocked forever — no
// concurrency needed. The timeout turns a regression into a failure instead
// of a hung suite.
func TestRenameFileTwoSpellingsOfOneFileRefused(t *testing.T) {
	dir := paths.Canonical(t.TempDir())
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(realDir, "f.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{Boundary: testBoundaryGuard(dir), WorkspaceFn: wsFn(dir)}
	raw := mustBoundaryJSON(t, map[string]string{
		"from": filepath.Join(dir, "link", "f.txt"),
		"to":   f,
	})
	done := make(chan error, 1)
	go func() { _, err := NewRenameFile(deps).Execute(context.Background(), raw); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "same path") {
			t.Fatalf("want a same-path refusal for two spellings of one file, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rename_file deadlocked on two spellings of one file — it took one non-reentrant mutex twice")
	}
}

// A case-only rename is a REAL operation, not a self-rename, and rename_file
// must still perform it. On a case-preserving filesystem `mv file.txt FILE.txt`
// stores the new spelling — correcting a file's casing is the whole point — so
// the same-path guard is keyed by paths.Canonical (the directory ENTRY) rather
// than by the case-folding lock key (the FILE). Keying it by the lock key would
// refuse this with a "same path" message naming something the caller never
// asked for, and leave no way to do it through the tool at all.
//
// The timeout still matters: locking uses the FOLDED key, so the pair takes one
// mutex, and letting the call through must not resurrect the double-acquisition
// TestRenameFileTwoSpellingsOfOneFileRefused pins.
func TestRenameFileCaseOnlyRenameIsPerformed(t *testing.T) {
	dir := paths.Canonical(t.TempDir())
	from := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(from, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(dir, "FILE.TXT")
	deps := WriteDeps{Boundary: testBoundaryGuard(dir), WorkspaceFn: wsFn(dir)}
	raw := mustBoundaryJSON(t, map[string]any{"from": from, "to": to, "overwrite": true})
	done := make(chan error, 1)
	go func() { _, err := NewRenameFile(deps).Execute(context.Background(), raw); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a case-only rename must be performed, not refused: %v", err)
		}
		if _, err := os.Lstat(to); err != nil {
			t.Fatalf("destination %q does not exist after the rename: %v", to, err)
		}
		// On a case-preserving filesystem the old spelling is gone because the
		// entry was renamed; on a case-sensitive one it is gone because it was
		// moved. Either way it must not still be there.
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if e.Name() == "file.txt" {
				t.Fatalf("the old spelling survived the rename: %v", ents)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rename_file deadlocked on a case-only rename — it took one non-reentrant mutex twice")
	}
}

// The precondition on the case-only rename, pinned so the CHANGELOG's claim is
// checkable rather than merely asserted: it needs overwrite: true. Where the
// filesystem folds case, renameFilePreconditions' os.Stat(to) finds the SOURCE
// through the fold and reports the destination as already existing. That check
// predates issue #346 and is left alone — it is the honest answer to "does
// something already live at this name?" on such a volume — but it means the
// obvious two-argument call does not do a casing fix.
func TestRenameFileCaseOnlyRenameNeedsOverwrite(t *testing.T) {
	dir := paths.Canonical(t.TempDir())
	from := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(from, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{Boundary: testBoundaryGuard(dir), WorkspaceFn: wsFn(dir)}
	raw := mustBoundaryJSON(t, map[string]string{"from": from, "to": filepath.Join(dir, "FILE.TXT")})
	_, err := NewRenameFile(deps).Execute(context.Background(), raw)

	if !caseVariantsAreOneFile(t, dir) {
		if err != nil {
			t.Fatalf("case-sensitive filesystem: the destination does not exist, so no overwrite is needed: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("want the destination-exists refusal without overwrite on a case-folding filesystem")
	}
	// Specifically the destination-exists refusal, NOT the same-path guard: that
	// guard firing here is the #346 regression this PR's fix removed, and a bare
	// "an error was returned" assertion would pass under it.
	if !strings.Contains(err.Error(), "exists") || strings.Contains(err.Error(), "same path") {
		t.Fatalf("want a destination-exists refusal, got %v", err)
	}
}

// TestCopyFileTwoSpellingsOfOneFileRefused is the copy_file half of the same
// regression — see TestRenameFileTwoSpellingsOfOneFileRefused.
func TestCopyFileTwoSpellingsOfOneFileRefused(t *testing.T) {
	dir := paths.Canonical(t.TempDir())
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(realDir, "f.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{Boundary: testBoundaryGuard(dir), WorkspaceFn: wsFn(dir)}
	raw := mustBoundaryJSON(t, map[string]string{
		"from": filepath.Join(dir, "link", "f.txt"),
		"to":   f,
	})
	done := make(chan error, 1)
	go func() { _, err := NewCopyFile(deps).Execute(context.Background(), raw); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "same path") {
			t.Fatalf("want a same-path refusal for two spellings of one file, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("copy_file deadlocked on two spellings of one file — it took one non-reentrant mutex twice")
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
