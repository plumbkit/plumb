package topology

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// indexer_resync_worktree_test.go covers the resync walk's fourth exclusion: a
// LINKED WORKTREE of the workspace's own repository. A worktree is a second full
// copy of the tree, so indexing each one multiplies the index and the walk for
// nothing new; a SUBMODULE is a different tree and stays indexed. The two are
// told apart by git's own bookkeeping — the gitdir: line of a .git FILE — never
// by a directory name, which is why the fixture is built with real git rather
// than with hand-written .git files: a hand-written pointer would only prove the
// fixture agrees with itself.

// requireGitForResync skips the calling test when git is not on PATH.
func requireGitForResync(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// gitIn runs one git command in dir, returning its output and failing the test
// on a non-zero exit.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// initCommittedRepo initialises a repository at dir and commits the working tree
// as it stands, giving it a local identity so the commit does not depend on the
// machine's git config. The caller writes the files first.
func initCommittedRepo(t *testing.T, dir string) {
	t.Helper()
	gitIn(t, dir, "init")
	gitIn(t, dir, "config", "user.email", "test@plumb.test")
	gitIn(t, dir, "config", "user.name", "Plumb Test")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-m", "fixture")
}

// worktreeWorkspace builds a workspace of the shape PLAN-491 was filed against:
// a repository with a tracked file at its root, one real submodule, and one real
// linked worktree, both living INSIDE the workspace.
//
// The submodule is added over the file protocol from a local repository, which
// needs no network — and produces exactly what `git submodule add` produces
// anywhere else: a nested checkout whose .git FILE records
// `gitdir: ../.git/modules/plumb`. The worktree is a real `git worktree add`,
// recording `gitdir: <root>/.git/worktrees/plumb-wt-1`.
func worktreeWorkspace(t *testing.T) (root string) {
	t.Helper()
	requireGitForResync(t)
	root = t.TempDir()
	writeIndexTree(t, root, map[string]string{"main.go": "package main\nfunc Main() {}\n"})
	initCommittedRepo(t, root)

	sub := t.TempDir()
	writeIndexTree(t, sub, map[string]string{"sub.go": "package sub\nfunc Sub() {}\n"})
	initCommittedRepo(t, sub)
	// protocol.file.allow: git 2.38+ refuses a file-protocol submodule by default,
	// and a local path is the only submodule source a test may assume.
	gitIn(t, root, "-c", "protocol.file.allow=always", "submodule", "add", sub, "plumb")

	gitIn(t, root, "worktree", "add", "-b", "wt-branch", filepath.Join(root, "plumb-wt-1"))
	return root
}

// pathsUnder returns the indexed paths at or below a workspace-relative prefix.
func pathsUnder(paths []string, prefix string) []string {
	var out []string
	for _, p := range paths {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			out = append(out, p)
		}
	}
	return out
}

// TestResync_SkipsLinkedWorktreeAndKeepsSubmodule is the rule end to end: with
// the default (index_worktrees off) the worktree contributes no rows at all,
// while the submodule beside it contributes its files; switching the opt-in on
// indexes the worktree too. Both halves matter: the second is what keeps the
// rule from being a blunt "any nested repository" exclusion, which would have
// silently dropped the ./plumb submodule this very repository is indexed with.
func TestResync_SkipsLinkedWorktreeAndKeepsSubmodule(t *testing.T) {
	root := worktreeWorkspace(t)
	idx, db := newTestIndexer(t, root)

	got := resyncPaths(t, idx, db)
	if !slices.Contains(got, "main.go") {
		t.Errorf("indexed = %v, want the tracked root file main.go", got)
	}
	if !slices.Contains(got, "plumb/sub.go") {
		t.Errorf("indexed = %v, want the submodule's plumb/sub.go — a submodule is "+
			"another tree and must stay indexed", got)
	}
	if under := pathsUnder(got, "plumb-wt-1"); len(under) != 0 {
		t.Errorf("indexed %v under the linked worktree; want none — a worktree is a "+
			"second copy of this tree, not another one", under)
	}

	// The fix must not be conditional on the worktree being gitignored: the bug
	// PLAN-491 was filed for is precisely a worktree that .gitignore does not
	// mention, so assert the walk really saw the directory and pruned it rather
	// than never reaching it.
	if _, err := os.Stat(filepath.Join(root, "plumb-wt-1", ".git")); err != nil {
		t.Fatalf("fixture: the worktree has no .git file to judge: %v", err)
	}

	idx.indexWorktrees = true
	got = resyncPaths(t, idx, db)
	if !slices.Contains(got, "plumb-wt-1/main.go") {
		t.Errorf("with [topology] index_worktrees on, indexed = %v; want the "+
			"worktree's plumb-wt-1/main.go", got)
	}
	if !slices.Contains(got, "plumb/sub.go") {
		t.Errorf("with index_worktrees on, the submodule's plumb/sub.go vanished: %v", got)
	}
}

// TestResync_DoesNotGuessAtUnexpectedGitFile pins the clause that keeps the rule
// from being a heuristic: only a gitdir under <root>/.git/worktrees/ is a linked
// worktree. A .git file pointing at a git directory elsewhere on the machine is
// some checkout nobody asked to exclude; a .git file that is not a gitdir
// pointer at all (or names no path) is one the walk cannot judge; and a gitdir
// under <root>/.git/modules/ is a submodule. Each must be walked.
//
// Here the pointers ARE hand-written, deliberately: the point is the shapes git
// does not produce, so there is no git command that could produce them.
func TestResync_DoesNotGuessAtUnexpectedGitFile(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "elsewhere.git")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeIndexTree(t, dir, map[string]string{
		"main.go":           "package main\nfunc Main() {}\n",
		"vendored/main.go":  "package vendored\nfunc V() {}\n",
		"vendored/.git":     "gitdir: " + outside + "\n",
		"malformed/main.go": "package malformed\nfunc M() {}\n",
		"malformed/.git":    "not a gitdir pointer at all\n",
		"modules/main.go":   "package modules\nfunc Mod() {}\n",
		"modules/.git":      "gitdir: ../.git/modules/modules\n",
		"empty/main.go":     "package empty\nfunc E() {}\n",
		"empty/.git":        "gitdir:\n",
		"relative/main.go":  "package relative\nfunc R() {}\n",
		"relative/.git":     "gitdir: ../.git/worktrees/relative\n",
	})
	idx, db := newTestIndexer(t, dir)

	got := resyncPaths(t, idx, db)
	for _, want := range []string{
		"main.go",
		"vendored/main.go",
		"malformed/main.go",
		"modules/main.go",
		"empty/main.go",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("indexed = %v, want %s — the walk must not guess that a .git file "+
				"it cannot classify is a worktree", got, want)
		}
	}
	// The relative form of the worktree pointer is the same rule, resolved
	// against the directory holding the .git file.
	if under := pathsUnder(got, "relative"); len(under) != 0 {
		t.Errorf("indexed %v under a .git file whose gitdir names "+
			"<root>/.git/worktrees/relative; want none", under)
	}
}

// TestResync_SkipsASubmoduleWorktree is the shape review round 1 caught (B1), and the case the
// test above cannot cover: agent worktrees in a workspace whose code lives in a submodule
// belong to THAT repository, so their gitdir is <root>/.git/modules/<sub>/worktrees/<name> —
// not <root>/.git/worktrees. The old prefix rule matched only the latter, so every plumb-ops
// agent worktree was indexed as another full copy of the repository, which is why PLAN-491 was
// filed. This test is the guard: a prefix rule cannot pass it.
func TestResync_SkipsASubmoduleWorktree(t *testing.T) {
	root := worktreeWorkspace(t)
	// A worktree OF THE SUBMODULE — what a plumb-ops agent worktree actually is. Real git, so
	// the gitdir line is the one git writes for a submodule worktree, not one this test invented.
	gitIn(t, filepath.Join(root, "plumb"), "worktree", "add", "-b", "wt-sub",
		filepath.Join(root, "plumb-wt-sub"))
	writeIndexTree(t, filepath.Join(root, "plumb-wt-sub"), map[string]string{
		"extra.go": "package sub\nfunc Extra() {}\n",
	})

	idx, db := newTestIndexer(t, root)
	got := resyncPaths(t, idx, db)

	if under := pathsUnder(got, "plumb-wt-sub"); len(under) != 0 {
		t.Errorf("a SUBMODULE's worktree was indexed: %v — its gitdir sits under "+
			".git/modules/plumb/worktrees, so it is a linked worktree of this workspace", under)
	}
	// The other half stays true: the submodule is content this workspace contains.
	if !slices.Contains(got, "plumb/sub.go") {
		t.Errorf("the submodule must stay indexed: %v", got)
	}
	if !slices.Contains(got, "main.go") {
		t.Errorf("the root's own file must stay indexed: %v", got)
	}
}
