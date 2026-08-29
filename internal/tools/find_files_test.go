package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// ── coverage carried over from list_files and list_directory ─────────────────
//
// These pin the behaviours the two folded tools owned. The one case NOT ported
// is list_files' hardcoded excludedDirs map (vendor/, node_modules/, dist/,
// build/, __pycache__): that map died with the tool, and gitignore confinement
// — already covered by TestFindFiles_RespectsGitignore — is now the only
// exclusion mechanism.

// listTree mirrors the fixture list_files' tests used, so the ported
// assertions run against the same shape of tree.
func listTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{
		"main.go", "main_test.go", "README.md", ".hidden",
		"sub/util.go", "sub/util_test.go",
	} {
		full := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runFindFiles(t *testing.T, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := NewFindFiles(nil).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("find_files(%v): %v", args, err)
	}
	return out
}

// TestFindFiles_OptionalPattern is the list_files default: no pattern lists
// every file under the root. It was find_files' one required parameter.
func TestFindFiles_OptionalPattern(t *testing.T) {
	root := listTree(t)
	out := runFindFiles(t, map[string]any{"path": root})
	for _, want := range []string{"main.go", "main_test.go", "README.md", "sub/util.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ".hidden") {
		t.Errorf("hidden entries stay excluded by default:\n%s", out)
	}
}

func TestFindFiles_NoPatternNoMatchMessage(t *testing.T) {
	out := runFindFiles(t, map[string]any{"path": t.TempDir()})
	if !strings.Contains(out, "No entries found under ") {
		t.Errorf("want the pattern-less no-match message, got:\n%s", out)
	}
}

func TestFindFiles_IncludeHidden(t *testing.T) {
	root := listTree(t)
	out := runFindFiles(t, map[string]any{"path": root, "include_hidden": true})
	if !strings.Contains(out, ".hidden") {
		t.Errorf("expected .hidden when include_hidden=true:\n%s", out)
	}
}

// TestFindFiles_MaxDepthStopsAtTheStatedLevel pins the depth contract the
// list_files alias relies on: max_depth=1 is one level, files included. The
// shared walk prunes directories at the limit but still visits the files inside
// the last directory it entered, so find_files applies its own depth check.
func TestFindFiles_MaxDepthStopsAtTheStatedLevel(t *testing.T) {
	root := listTree(t)
	out := runFindFiles(t, map[string]any{"path": root, "max_depth": 1})
	if !strings.Contains(out, "main.go") {
		t.Errorf("top-level files must survive max_depth=1:\n%s", out)
	}
	if strings.Contains(out, "sub/util.go") {
		t.Errorf("sub/util.go is one level too deep for max_depth=1:\n%s", out)
	}
}

// TestFindFiles_DetailsSingleLevel is the list_directory rendering, reached the
// way its alias reaches it: one level, both types, details on.
func TestFindFiles_DetailsSingleLevel(t *testing.T) {
	root := listTree(t)
	out := runFindFiles(t, map[string]any{
		"path": root, "max_depth": 1, "type": "any", "include_details": true,
	})
	for _, want := range []string{root, "[DIR]  sub", "[FILE] main.go", "1 directory, 3 files"} {
		if !strings.Contains(out, want) {
			t.Errorf("detailed listing missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "result(s)") {
		t.Errorf("detailed listing should use the directory/file tally, not the result count:\n%s", out)
	}
}

func TestFindFiles_DetailsEmptyDirectory(t *testing.T) {
	out := runFindFiles(t, map[string]any{
		"path": t.TempDir(), "max_depth": 1, "type": "any", "include_details": true,
	})
	if !strings.Contains(out, "(empty)") {
		t.Errorf("want list_directory's (empty) rendering, got:\n%s", out)
	}
}

func TestFindFiles_DetailsSymlinkShowsTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	out := runFindFiles(t, map[string]any{
		"path": dir, "max_depth": 1, "type": "any", "include_details": true,
	})
	if !strings.Contains(out, "[LINK]") {
		t.Errorf("expected a [LINK] entry, got:\n%s", out)
	}
	if !strings.Contains(out, "link.txt -> real.txt") {
		t.Errorf("expected symlink target annotation, got:\n%s", out)
	}
}

// TestFindFiles_SortOrders pins list_directory's three orders, applied to the
// flat result list.
func TestFindFiles_SortOrders(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int, age time.Duration) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write("b_big.txt", 4096, 3*time.Hour)
	write("a_small.txt", 8, 1*time.Hour)
	if err := os.MkdirAll(filepath.Join(dir, "zdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Age the directory past both files so the modified order is unambiguous:
	// it is the one order with no dirs-first rule, and a just-created directory
	// would otherwise legitimately sort first.
	old := time.Now().Add(-5 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "zdir"), old, old); err != nil {
		t.Fatal(err)
	}

	firstLine := func(out string) string {
		for line := range strings.SplitSeq(out, "\n") {
			if line != "" && line != dir {
				return line
			}
		}
		return ""
	}

	tests := []struct {
		sortBy string
		want   string
	}{
		{"name", "zdir"},            // directories first, then path
		{"size", "zdir"},            // directories first, then largest
		{"modified", "a_small.txt"}, // newest first, no dirs-first rule
	}
	for _, tt := range tests {
		t.Run(tt.sortBy, func(t *testing.T) {
			out := runFindFiles(t, map[string]any{
				"path": dir, "max_depth": 1, "type": "any", "sort_by": tt.sortBy,
			})
			if got := firstLine(out); !strings.Contains(got, tt.want) {
				t.Errorf("sort_by=%s first entry = %q, want it to be %q:\n%s", tt.sortBy, got, tt.want, out)
			}
		})
	}

	out := runFindFiles(t, map[string]any{"path": dir, "sort_by": "size"})
	if got := firstLine(out); !strings.Contains(got, "b_big.txt") {
		t.Errorf("sort_by=size over files only should lead with the largest, got %q:\n%s", got, out)
	}
}

// TestFindFiles_FilePathListsParentWithANote is F1. Pointing find_files at a
// FILE has always walked its parent — a caller who names a file usually means
// "around here" — and the retired list_directory hard-errored on the same
// input. Keeping the walk is the compatible choice for canonical callers; the
// note is what stops it being a silently different answer for the alias's.
func TestFindFiles_FilePathListsParentWithANote(t *testing.T) {
	root := listTree(t)
	file := filepath.Join(root, "main.go")

	out := runFindFiles(t, map[string]any{"path": file})
	wantNote := "note: " + file + " is a file — listing its parent directory " + root + ".\n\n"
	if !strings.HasPrefix(out, wantNote) {
		t.Errorf("want the redirect announced up front:\nwant prefix %q\ngot:\n%s", wantNote, out)
	}
	if !strings.Contains(out, "README.md") {
		t.Errorf("the parent listing must follow the note:\n%s", out)
	}
}

// TestFindFiles_FilePathNoteSurvivesAnEmptyResult keeps the note on the branch
// that renders no rows at all — the one where a caller has the least other
// evidence about which directory they were answered from.
func TestFindFiles_FilePathNoteSurvivesAnEmptyResult(t *testing.T) {
	root := listTree(t)
	out := runFindFiles(t, map[string]any{"path": filepath.Join(root, "main.go"), "pattern": "*.nope"})
	if !strings.Contains(out, "is a file — listing its parent directory") {
		t.Errorf("the note must survive the no-match branch:\n%s", out)
	}
}

// TestFindFiles_MaxDepthRejectsNonPositive is F6's canonical half. find_files
// declares max_depth "minimum": 1 and reads 0 as unlimited, so an unchecked 0
// INVERTS the caller's intent — the shallowest request there is, answered with
// the whole tree. A clean rejection is the only honest reading.
func TestFindFiles_MaxDepthRejectsNonPositive(t *testing.T) {
	root := listTree(t)
	for _, depth := range []int{0, -1} {
		raw, err := json.Marshal(map[string]any{"path": root, "max_depth": depth})
		if err != nil {
			t.Fatal(err)
		}
		out, err := NewFindFiles(nil).Execute(context.Background(), raw)
		if err == nil {
			t.Fatalf("max_depth=%d must be rejected, got:\n%s", depth, out)
		}
		if !strings.Contains(err.Error(), "max_depth must be >= 1") {
			t.Errorf("max_depth=%d error = %v, want it to name the range", depth, err)
		}
	}
}

// TestFindFiles_ExcludesDotGitEvenWhenHidden is F3. include_hidden asks for
// dotfiles, not for the object database — and find_files' own description
// promises "no .git/", which only .gitignore-independent exclusion can keep.
func TestFindFiles_ExcludesDotGitEvenWhenHidden(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, ".git", "objects", "ab"), 0o755))
	must(os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, ".git", "objects", "ab", "cdef"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(root, ".env"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(root, "main.go"), []byte("x"), 0o644))

	out := runFindFiles(t, map[string]any{"path": root, "include_hidden": true, "type": "any"})
	if strings.Contains(out, ".git") {
		t.Errorf(".git must be excluded whatever include_hidden says:\n%s", out)
	}
	for _, want := range []string{".env", "main.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("excluding .git must not disturb the rest of include_hidden (missing %q):\n%s", want, out)
		}
	}
}

// TestFindFiles_TruncationReportsTheTally is F2's second half: a truncation
// note that REPLACES the count drops the one number every other summary branch
// reports — and for a detailed listing it would drop the directory/file split
// entirely.
func TestFindFiles_TruncationReportsTheTally(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.go", "b.go", "c.go", "d.go"} {
		if err := os.WriteFile(filepath.Join(root, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := runFindFiles(t, map[string]any{"path": root, "max_results": 2})
	if !strings.Contains(out, "2 result(s) (truncated at 2 results") {
		t.Errorf("a truncated summary must lead with the tally:\n%s", out)
	}

	detailed := runFindFiles(t, map[string]any{
		"path": root, "max_results": 2, "type": "any", "include_details": true,
	})
	if !strings.Contains(detailed, "0 directories, 2 files (truncated at 2 results") {
		t.Errorf("a truncated detailed listing must keep its directory/file tally:\n%s", detailed)
	}
}

// TestFindFiles_ExactlyMaxResultsIsNotTruncated is F13. Truncation means the
// walk STOPPED SHORT. A result set that happens to land exactly on max_results
// having exhausted the tree is complete, and calling it truncated sends the
// caller narrowing a search that had nothing left to find.
func TestFindFiles_ExactlyMaxResultsIsNotTruncated(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.go", "b.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(root, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := runFindFiles(t, map[string]any{"path": root, "max_results": 3})
	if strings.Contains(out, "truncated") {
		t.Errorf("exactly max_results matches with nothing left is complete:\n%s", out)
	}
	if !strings.Contains(out, "3 result(s)") {
		t.Errorf("want the plain count, got:\n%s", out)
	}
}

// TestFindFiles_TruncationNoteIsSingularAtOne guards the plural the fixed
// summary now carries.
func TestFindFiles_TruncationNoteIsSingularAtOne(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := runFindFiles(t, map[string]any{"path": root, "max_results": 1})
	if !strings.Contains(out, "truncated at 1 result —") {
		t.Errorf("want the singular form at max_results=1, got:\n%s", out)
	}
}

// TestNewFindFileHit_DirectorySizeIsZero is F16. A directory's stat size is its
// inode's own bookkeeping; the detailed rendering leaves the column blank for
// one, so sort_by="size" was ranking directories by a number the caller is
// never shown.
func TestNewFindFileHit_DirectorySizeIsZero(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Enough entries that the directory inode is unlikely to report size 0 by
	// accident, so a passing assertion means the code chose zero.
	for i := range 200 {
		if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%03d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	h := newFindFileHit(sub, "sub", entries[0], true, true)
	if h.size != 0 {
		t.Errorf("directory hit size = %d, want 0", h.size)
	}
	if h.modified == 0 {
		t.Error("a directory hit must still carry its modified time")
	}
}
