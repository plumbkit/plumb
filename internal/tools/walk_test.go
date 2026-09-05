package tools

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// ── walk ────────────────────────────────────────────────────────────────────

func TestWalk_RespectsGitignore(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(os.MkdirAll(filepath.Join(dir, "vendor", "pkg"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "vendor", "pkg", "lib.go"), []byte("package p"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "debug.log"), []byte("log"), 0o644))
	must(os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("vendor/\n*.log\n"), 0o644))

	var visited []string
	opts := walkOptions{root: dir, respectIgnore: true}
	if err := walk(context.Background(), opts, func(path string, d fs.DirEntry, _ int) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			visited = append(visited, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, v := range visited {
		if v == "vendor/pkg/lib.go" {
			t.Error("vendor/pkg/lib.go should be ignored")
		}
		if v == "debug.log" {
			t.Error("debug.log should be ignored")
		}
	}

	found := false
	for _, v := range visited {
		if v == "src/main.go" {
			found = true
		}
	}
	if !found {
		t.Error("src/main.go should be visited")
	}
}

func TestWalk_HiddenFiles(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "visible"), []byte("x"), 0o644))

	var noHidden, withHidden []string
	_ = walk(context.Background(), walkOptions{root: dir}, func(path string, d fs.DirEntry, _ int) error {
		if !d.IsDir() {
			noHidden = append(noHidden, filepath.Base(path))
		}
		return nil
	})
	_ = walk(context.Background(), walkOptions{root: dir, includeHidden: true}, func(path string, d fs.DirEntry, _ int) error {
		if !d.IsDir() {
			withHidden = append(withHidden, filepath.Base(path))
		}
		return nil
	})

	for _, n := range noHidden {
		if n == ".hidden" {
			t.Error("hidden file should not appear when includeHidden=false")
		}
	}
	found := false
	for _, n := range withHidden {
		if n == ".hidden" {
			found = true
		}
	}
	if !found {
		t.Error(".hidden should appear when includeHidden=true")
	}
}

// TestWalk_MaxDepth pins the depth contract EXACTLY: maxDepth N visits entries
// at depths 0..N-1 and nothing below. The assertion is an exact set, not an
// allowlist, because it doubles as the pruning proof — walkDir calls fn for
// every entry it reads, so "a/a.go was never visited" is the same statement as
// "a/ was never opened". The old walk descended into a/ and then discarded
// everything it found there, paying a ReadDir and a .gitignore load per
// top-level directory for a one-level listing.
func TestWalk_MaxDepth(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "a", "b", "c"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "root.go"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "a", "a.go"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "a", "b", "b.go"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "a", "b", "c", "c.go"), []byte("x"), 0o644))

	visit := func(maxDepth int) []string {
		var visited []string
		_ = walk(context.Background(), walkOptions{root: dir, maxDepth: maxDepth}, func(path string, d fs.DirEntry, _ int) error {
			if !d.IsDir() {
				rel, _ := filepath.Rel(dir, path)
				visited = append(visited, filepath.ToSlash(rel))
			}
			return nil
		})
		sort.Strings(visited)
		return visited
	}

	for _, tc := range []struct {
		maxDepth int
		want     []string
	}{
		{1, []string{"root.go"}},
		{2, []string{"a/a.go", "root.go"}},
		{3, []string{"a/a.go", "a/b/b.go", "root.go"}},
	} {
		if got := visit(tc.maxDepth); !slices.Equal(got, tc.want) {
			t.Errorf("maxDepth=%d visited %v, want exactly %v", tc.maxDepth, got, tc.want)
		}
	}
}

// TestWalk_NeverEntersDotGit is the hard exclusion every walk consumer relies
// on. include_hidden means "show me dotfiles", not "show me the object
// database": before this, a hidden-inclusive walk would happily hand back
// thousands of loose objects, and find_replace would rewrite inside them.
func TestWalk_NeverEntersDotGit(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "nested", ".git"), 0o755))
	must(os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, ".git", "objects", "blob"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "nested", ".git", "HEAD"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, ".env"), []byte("x"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "wt"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "wt", ".git"), []byte("gitdir: ../.git/worktrees/wt\n"), 0o644))

	for _, includeHidden := range []bool{false, true} {
		var seen []string
		_ = walk(context.Background(), walkOptions{root: dir, includeHidden: includeHidden},
			func(path string, _ fs.DirEntry, _ int) error {
				rel, _ := filepath.Rel(dir, path)
				seen = append(seen, filepath.ToSlash(rel))
				return nil
			})
		for _, s := range seen {
			if s == "wt/.git" {
				continue // a worktree gitdir POINTER is a plain file; only the directory is excluded
			}
			if strings.Contains(s, ".git") {
				t.Errorf("includeHidden=%v: walk entered .git (%s); the exclusion is unconditional", includeHidden, s)
			}
		}
		hasPointer := slices.Contains(seen, "wt/.git")
		if hasPointer != includeHidden {
			t.Errorf("includeHidden=%v: wt/.git pointer file present = %v, want %v — the exclusion is directory-scoped, the file follows the ordinary hidden policy",
				includeHidden, hasPointer, includeHidden)
		}
		hasEnv := slices.Contains(seen, ".env")
		if hasEnv != includeHidden {
			t.Errorf("includeHidden=%v: .env present = %v, want %v — excluding .git must not disturb the hidden-file policy",
				includeHidden, hasEnv, includeHidden)
		}
	}
}
