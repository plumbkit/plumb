package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// worktrees.go answers which directories under a workspace are linked worktrees
// of it (`git worktree add`): second checkouts of code the root already holds,
// which the topology index and the file watcher leave out. Agents create and
// remove them by the dozen, and each is a full copy of the tree.
//
// Concurrency: stateless and read-only; safe for concurrent use.

// LinkedWorktreeOf reports whether dir is a verified linked worktree of the
// repository checked out at root, or of one of root's submodules.
//
// Everything is verified the way CheckoutAt verifies it — commondir plus git's
// own back-link — so a hand-written `.git` file proves nothing and is not
// skipped. A submodule checkout (no commondir) is content the root contains and
// is never a worktree here, whatever its path is called. A worktree of an
// unrelated repository that happens to live under root is not root's either.
func LinkedWorktreeOf(root, dir string) bool {
	// The cheap miss first: most directories hold no .git at all, and the
	// verification below resolves symlinks.
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	c, ok := CheckoutAt(dir)
	if !ok || samePath(c.GitDir, c.CommonDir) {
		return false // no verified checkout, a main checkout, or a submodule
	}
	r, ok := CheckoutAt(root)
	if !ok {
		return false
	}
	return samePath(c.CommonDir, r.CommonDir) || nestedGitDir(c.CommonDir, r.CommonDir)
}

// LinkedWorktreesOf lists the checkouts git's own bookkeeping records as linked
// worktrees of root's repository and of its submodules: every
// <git dir>/worktrees/<id>/gitdir back-link, read from the repository side. It
// lists a worktree whose checkout is being deleted, too — `git worktree remove`
// deletes the checkout before its admin directory, and a plain `rm -rf` leaves
// the admin directory until `git worktree prune` — which a check of the
// checkout's own .git cannot, once that file is gone. Paths are canonical.
// Entries that are not directories below root are left out.
func LinkedWorktreesOf(root string) []string {
	r, ok := CheckoutAt(root)
	if !ok {
		return nil
	}
	canonRoot := Canonical(root)
	var out []string
	for _, gitDir := range repositoryGitDirs(r.CommonDir) {
		admins, err := os.ReadDir(filepath.Join(gitDir, "worktrees"))
		if err != nil {
			continue
		}
		for _, a := range admins {
			if !a.IsDir() {
				continue
			}
			admin := filepath.Join(gitDir, "worktrees", a.Name())
			data, err := os.ReadFile(filepath.Join(admin, "gitdir"))
			if err != nil {
				continue
			}
			link := strings.TrimSpace(string(data))
			if link == "" {
				continue
			}
			wt := Canonical(filepath.Dir(resolveAgainst(admin, link)))
			if rel, err := filepath.Rel(canonRoot, wt); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				out = append(out, wt)
			}
		}
	}
	return out
}

// repositoryGitDirs is common plus every submodule git directory git keeps
// beneath it (<dir>/modules/<name…>, nested submodules in turn). A submodule's
// name may contain '/', so a directory under modules/ that is not itself a git
// directory is a name component to descend through. Nothing else in a git
// directory (objects, refs, logs) is walked.
func repositoryGitDirs(common string) []string {
	out := []string{common}
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			child := filepath.Join(dir, e.Name())
			if isGitDir(child) {
				out = append(out, child)
				walk(filepath.Join(child, "modules"))
			} else {
				walk(child)
			}
		}
	}
	walk(filepath.Join(common, "modules"))
	return out
}
