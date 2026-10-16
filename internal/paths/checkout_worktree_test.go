package paths

import (
	"path/filepath"
	"testing"
)

// TestLinkedWorktreeOf covers both answers: the checkouts the topology index
// and the file watcher leave out (a second checkout of code root already
// holds) and the ones they must keep.
func TestLinkedWorktreeOf(t *testing.T) {
	root := mainCheckout(t, t.TempDir())
	common := filepath.Join(root, ".git")

	ownWT := filepath.Join(root, "plumb-wt-a")
	linkedWorktree(t, common, "plumb-wt-a", ownWT, false)

	sub := filepath.Join(root, "plumb")
	subGit := submodule(t, root, "plumb", sub, "../../../plumb")
	subWT := filepath.Join(root, "plumb-wt-b")
	linkedWorktree(t, subGit, "plumb-wt-b", subWT, true)

	// A submodule at a path spelling the worktree shape is still a submodule.
	shaped := filepath.Join(root, "libs", "worktrees", "foo")
	submodule(t, root, "libs/worktrees/foo", shaped, "../../../../../libs/worktrees/foo")

	// An unrelated repository living under root, and a worktree of it.
	other := mainCheckout(t, filepath.Join(root, "repos", "other"))
	otherWT := filepath.Join(root, "repos", "other-wt")
	linkedWorktree(t, filepath.Join(other, ".git"), "other-wt", otherWT, false)

	// A hand-written pointer in the worktree shape that git's bookkeeping
	// does not back.
	forged := filepath.Join(root, "forged")
	write(t, filepath.Join(forged, ".git"), "gitdir: "+filepath.Join(common, "worktrees", "forged")+"\n")

	for _, tc := range []struct {
		name string
		dir  string
		want bool
	}{
		{"a worktree of the root repository", ownWT, true},
		{"a worktree of a submodule of root", subWT, true},
		{"the submodule itself", sub, false},
		{"a submodule whose path holds a worktrees segment", shaped, false},
		{"an unrelated repository under root", other, false},
		{"a worktree of an unrelated repository under root", otherWT, false},
		{"a forged worktree pointer", forged, false},
		{"a plain directory", filepath.Join(root, "repos"), false},
		{"the root itself", root, false},
	} {
		if got := LinkedWorktreeOf(root, tc.dir); got != tc.want {
			t.Errorf("%s: LinkedWorktreeOf = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestLinkedWorktreeOf_NeedsARepositoryRoot: under a root that is no checkout
// nothing is anybody's worktree.
func TestLinkedWorktreeOf_NeedsARepositoryRoot(t *testing.T) {
	plain := t.TempDir()
	repo := mainCheckout(t, filepath.Join(plain, "repo"))
	wt := filepath.Join(plain, "wt")
	linkedWorktree(t, filepath.Join(repo, ".git"), "wt", wt, false)
	if LinkedWorktreeOf(plain, wt) {
		t.Error("a worktree was classified against a root that is not a checkout")
	}
	if !LinkedWorktreeOf(repo, wt) {
		t.Error("control: the worktree is its repository's, when that repository is the root")
	}
}
