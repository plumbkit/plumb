package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// mutationtest_gitdisk.go is mutationtest_root.go's fallback for when git cannot
// answer for a directory: the nearest .git is read straight from the disk.

// diskProbe reads the nearest .git at or above dir straight from the disk — the
// fallback for when git cannot answer (reason is git's words). A .git DIRECTORY
// marks a main work-tree whose common directory is that .git. A .git FILE is a
// link, "gitdir: <dir>": the linked directory belongs to a worktree exactly when
// it holds a `commondir` file naming the common directory (relative to it), which
// is how git itself tells a worktree's git directory from a submodule's. No .git
// anywhere above is no repository. A link that cannot be read is placeUnknown.
func diskProbe(dir, reason string) gitProbe {
	for d := paths.Canonical(dir); ; {
		dotGit := filepath.Join(d, ".git")
		if info, err := os.Lstat(dotGit); err == nil {
			tree, ok := diskTree(d, dotGit, info)
			if !ok {
				return gitProbe{place: placeUnknown, reason: reason}
			}
			return gitProbe{place: placeTree, tree: tree, reason: reason, approx: true}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return gitProbe{place: placeNoRepo, reason: reason, approx: true}
		}
		d = parent
	}
}

// diskTree describes the work-tree rooted at top from its .git entry (info), or
// reports false when a .git link cannot be read.
func diskTree(top, dotGit string, info os.FileInfo) (gitTree, bool) {
	if info.IsDir() {
		return gitTree{top: top, common: paths.Canonical(dotGit)}, true
	}
	gitDir, ok := readGitLink(dotGit, top)
	if !ok {
		return gitTree{}, false
	}
	data, err := readGoConfigFile(filepath.Join(gitDir, "commondir"))
	if errors.Is(err, os.ErrNotExist) {
		// A submodule's git directory, not a worktree's — unless it is a submodule
		// checked out INSIDE a linked worktree (<common>/worktrees/<id>/modules/…),
		// whose twin is the main checkout's copy. Without git to name the
		// superproject, that shape is the evidence, and it counts as linked.
		return gitTree{top: top, common: gitDir, linked: inWorktreeModules(gitDir)}, true
	}
	if err != nil {
		return gitTree{}, false
	}
	common := strings.TrimSpace(string(data))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	return gitTree{top: top, common: paths.Canonical(common), linked: true}, true
}

// inWorktreeModules reports whether gitDir lies under a "worktrees/<id>/modules/"
// segment: the git directory of a submodule checked out in a linked worktree.
func inWorktreeModules(gitDir string) bool {
	parts := strings.Split(filepath.ToSlash(gitDir), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "worktrees" && parts[i+2] == "modules" {
			return true
		}
	}
	return false
}

// readGitLink reads a .git file's "gitdir: <dir>" line and returns the directory
// it names, made absolute against dir (where the .git file is) and resolved.
func readGitLink(dotGit, dir string) (string, bool) {
	data, err := readGoConfigFile(dotGit)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", false
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return paths.Canonical(target), true
}
