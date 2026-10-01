package tools

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// git_own_writes.go keeps plumb's own git operations from being reported as a
// peer's edits (#529).
//
// read_file warns "changed on disk since plumb last wrote it this session — a
// peer or external process may have edited it" when a file plumb wrote has a
// newer mtime than the WriteTracker recorded. A switch, merge, restore or stash
// pop made THROUGH the git tool rewrites such files too, so the next read blamed
// a peer for plumb's own operation.
//
// runGit therefore takes a before-image of the session's recorded files in the
// repository (WriteTracker.unchangedUnder) once it holds the per-repository
// lock, and afterwards re-records those the operation changed. Three properties
// keep the warning honest:
//
//   - A file a peer had ALREADY changed before the operation is not in the
//     before-image, so its warning survives whatever git then does to it.
//   - A change after the operation is newer than the refreshed record, so it
//     warns exactly as before.
//   - A change DURING an operation that runs repository hooks (merge's
//     pre-merge-commit, switch's post-checkout, …) is re-recorded only when git
//     produced it: the file must now match the index, or be a conflicted path
//     git wrote markers into. A hook's or peer's write to a file git did not
//     produce leaves the working tree differing from the index, and keeps its
//     warning (gitProducedPaths).
//
// commit is excluded altogether: it never rewrites the working tree itself, and
// its hooks can run for minutes. The hook-free working-tree verbs (restore,
// reset, stash, clean, mv) need no filter — only git and a peer racing the
// operation could write in that window — and would not pass one: `stash pop`
// and `restore --source` leave files that differ from the index by design.

// trackOwnGitWrites takes the before-image for a git call and returns the
// refresh to run once the child has exited (a no-op for a call that cannot
// rewrite the working tree). nil-safe on writes.
func trackOwnGitWrites(writes *WriteTracker, repoRoot, sub string, tier gitTier) func() {
	if writes == nil || !rewritesWorkTree(sub, tier) {
		return func() {}
	}
	before := writes.unchangedUnder(repoRoot)
	return func() {
		now := changedSince(before)
		if len(now) > 0 && runsRepoHooks(sub) {
			now = gitProducedPaths(repoRoot, now)
		}
		writes.rerecord(before, now)
	}
}

// rewritesWorkTree reports whether a git call can itself change files in the
// working tree, and so needs its effect on the session's written files
// re-recorded. Reads never do; nor do the index- and ref-only writes (add,
// commit, branch and tag create) or fetch and push, which touch only refs.
func rewritesWorkTree(sub string, tier gitTier) bool {
	if tier == tierRead || tier == tierReject {
		return false
	}
	switch sub {
	case "add", "commit", "branch", "tag", "fetch", "push":
		return false
	}
	return true
}

// runsRepoHooks reports whether a working-tree-rewriting git call can run a
// repository hook while it works — so a write during it is not necessarily
// git's.
func runsRepoHooks(sub string) bool {
	switch sub {
	case "switch", "checkout", "merge", "pull", "rebase", "cherry-pick", "revert":
		return true
	}
	return false
}

// gitProducedPaths narrows changed to the paths whose current content git
// produced: clean against the index (checked out, merged, or removed by git),
// or unmerged (conflict markers git wrote). Untracked, ignored, and
// worktree-modified paths are left out. --ignored with --untracked-files=all
// lists ignored FILES individually, never just their directory, so a file under
// an ignored directory cannot slip through as unlisted. Any failure to ask git leaves
// everything out — keeping a warning is the safe side of this call.
func gitProducedPaths(repoRoot string, changed map[string]int64) map[string]int64 {
	rootKey := lockPathKey(repoRoot)
	args := []string{gitNoOptionalLocks, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored", "--"}
	for key := range changed {
		rel, err := filepath.Rel(rootKey, key)
		if err != nil {
			return nil
		}
		// Keys are canonical and may be case-folded, so match case-insensitively
		// and literally (no glob in a file name can widen the match).
		args = append(args, ":(literal,icase)"+filepath.ToSlash(rel))
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	notGits := map[string]bool{}
	entries := strings.Split(string(out), "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		xy, path := e[:2], e[3:]
		if xy[0] == 'R' || xy[0] == 'C' {
			i++ // porcelain -z follows a rename/copy with its source path
		}
		if !statusIsGits(xy) {
			notGits[lockPathKey(filepath.Join(repoRoot, filepath.FromSlash(path)))] = true
		}
	}
	kept := make(map[string]int64, len(changed))
	for key, mtime := range changed {
		if !notGits[key] {
			kept[key] = mtime
		}
	}
	return kept
}

// statusIsGits reports whether a porcelain status code describes content git
// produced: unmerged (either side U, or both added/deleted), or a working tree
// that matches the index.
func statusIsGits(xy string) bool {
	if xy[0] == 'U' || xy[1] == 'U' || xy == "AA" || xy == "DD" {
		return true
	}
	return xy[1] == ' '
}
