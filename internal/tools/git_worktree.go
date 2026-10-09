package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// git_worktree.go holds the one check `git worktree` needs beyond its tier
// (PLAN-454). Adding and removing worktrees is how a reviewer inspects a branch
// without disturbing the checkout they are working in, which is why the read
// tier admits `worktree list` at all; the guard below covers the direction that
// destroys work.

// checkGitWorktreeRemove refuses a `git worktree remove` that would discard
// uncommitted work unless the caller confirmed.
//
// git already refuses a worktree holding modified or untracked files, so the
// forms that reach past it are the ones this guard exists for:
//
//   - an explicit -f/--force, which discards those files and removes even a
//     locked worktree;
//   - a target whose state plumb cannot prove clean, where a caller who thought
//     they were removing an empty probe directory should hear why plumb paused
//     rather than reading git's refusal as a plumb bug.
//
// The refusal names what would be lost and both ways forward. That is the
// decision confirm:true asks the caller to make knowingly; a bare "refused"
// would only teach them to pass confirm:true by reflex.
//
// The path git would resolve is the path resolved here, against the same
// repository root, so a relative target inside a subdirectory of the repo is
// checked as git would remove it.
func checkGitWorktreeRemove(ctx context.Context, a gitToolArgs) error {
	if a.Subcommand != "worktree" || len(a.Args) == 0 || a.Args[0] != "remove" || a.Confirm {
		return nil
	}
	force := worktreeGrammar.has(a.Args, "f", "force")
	positionals := worktreeGrammar.positionals(a.Args)
	// positionals[0] is the "remove" verb itself, so the target is next. A call
	// with no target is git's to refuse, in git's words.
	if len(positionals) < 2 {
		return nil
	}
	target := positionals[1]
	root, err := findGitRoot(a.Repo)
	if err != nil {
		// No repository to resolve against: git's own failure is the better report.
		return nil
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	dirty := dirtyBasenamesInDir(ctx, target, nil, false)
	if len(dirty) == 0 {
		if !force {
			return nil
		}
		return fmt.Errorf("git worktree remove --force %s: --force removes a worktree git would otherwise protect "+
			"(a locked one, or one whose ignored files would be deleted with it). "+
			"Re-run with confirm:true if that is what you mean", target)
	}
	lost := "that worktree holds " + dirtySummary(dirty)
	discard := plural(len(dirty), "it", "them")
	if force {
		return fmt.Errorf("git worktree remove --force %s: %s, and --force removes it anyway, discarding %s. "+
			"Commit or stash there first, or re-run with confirm:true to discard %s deliberately",
			target, lost, discard, discard)
	}
	return fmt.Errorf("git worktree remove %s: %s, and removing it discards %s. "+
		"Commit or stash there first, or re-run with confirm:true to discard %s deliberately",
		target, lost, discard, discard)
}

// dirtySummary renders a dirty-file set for a refusal: the count, then up to
// three names in a stable order, then how many more the caller is not being
// shown. Names are basenames as git reported them, which is what a caller needs
// to recognise the work they are about to lose.
func dirtySummary(dirty map[string]bool) string {
	names := make([]string, 0, len(dirty))
	for name := range dirty {
		names = append(names, name)
	}
	sort.Strings(names)
	shown := names
	if len(shown) > 3 {
		shown = shown[:3]
	}
	summary := fmt.Sprintf("%d uncommitted %s (%s", len(names), plural(len(names), "entry", "entries"),
		strings.Join(shown, ", "))
	if len(names) > len(shown) {
		summary += fmt.Sprintf(", … %d more", len(names)-len(shown))
	}
	return summary + ")"
}

// plural picks the singular or plural word for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
