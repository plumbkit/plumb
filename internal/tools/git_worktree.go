package tools

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/plumbkit/plumb/internal/textfmt"
)

// git_worktree.go holds the checks `git worktree` needs beyond its tier
// (PLAN-454). Adding and removing worktrees is how a reviewer inspects a branch
// without disturbing the checkout they are working in, which is why the read
// tier admits `worktree list` at all; everything here covers the directions that
// write: where a worktree may be created or deleted at all, and the one
// sub-verb that destroys work.

// checkGitWorktree runs the refusals a `worktree` call needs before git starts:
// the workspace boundary on every path it creates, moves or deletes, then the
// guards on removal.
func (t *Git) checkGitWorktree(ctx context.Context, a gitToolArgs) error {
	if a.Subcommand != "worktree" || len(a.Args) == 0 {
		return nil
	}
	if err := t.checkWorktreeBoundary(ctx, a); err != nil {
		return err
	}
	return t.checkWorktreeRemove(ctx, a)
}

// worktreeConfinedPaths returns the arguments of a worktree sub-verb that name a
// path on disk: the directory `add` creates, the worktree `remove` deletes, and
// both the worktree `move` leaves and the path it enters. `list`, `lock`,
// `unlock`, `prune` and `repair` name no path plumb must confine — lock and
// unlock address a worktree the repository already knows, and the rest touch
// only git's own administration inside the repository.
//
// `add`'s optional trailing argument is a commit-ish, not a path, so it is not
// returned: `git worktree add [-b branch] <path> [<commit-ish>]`.
func worktreeConfinedPaths(args []string) []string {
	pos := worktreeGrammar.positionals(args)
	if len(pos) < 2 {
		return nil
	}
	switch pos[0] {
	case "add", "remove":
		return pos[1:2]
	case "move":
		return pos[1:min(3, len(pos))]
	default:
		return nil
	}
}

// checkWorktreeBoundary confines those paths to the workspace through the same
// boundary every other tool uses — symlinks resolved, no lexical-only ".."
// (#264) — so `worktree add -b far /tmp/x` is refused instead of creating a
// checkout outside the project, and `remove` cannot delete one.
//
// confirm deliberately does NOT lift this. The boundary is not a risk the caller
// can accept on plumb's behalf: every other plumb tool refuses outside paths
// outright, and a caller who really wants a worktree out there can still create
// it with native git. The join below only makes a relative path absolute the way
// git resolves it (against the repository root); the boundary, not this lexical
// form, decides.
func (t *Git) checkWorktreeBoundary(ctx context.Context, a gitToolArgs) error {
	root := ""
	for _, p := range worktreeConfinedPaths(a.Args) {
		target := p
		if !filepath.IsAbs(target) {
			if root == "" {
				var err error
				if root, err = findGitRoot(a.Repo); err != nil {
					// No repository to resolve against: git's own failure is the better report.
					return nil
				}
			}
			target = filepath.Join(root, target)
		}
		if err := t.deps.checkBoundary(ctx, target); err != nil {
			return fmt.Errorf("git worktree: %w", err)
		}
	}
	return nil
}

// checkWorktreeRemove refuses a `git worktree remove` that would discard work
// unless the caller confirmed.
//
// git already refuses a worktree holding modified or untracked files, so the
// forms that reach past it are the ones this guard exists for:
//
//   - an explicit -f/--force. Ignored files are NOT the difference — git deletes
//     those without --force, since they are neither modified nor untracked — but
//     --force does override the protection on a LOCKED worktree and on one
//     holding a submodule checkout.
//   - a worktree on a detached HEAD whose commits no branch, tag or remote
//     reaches. Its HEAD reflog goes with the directory, so removing it leaves
//     those commits dangling with nothing naming them.
//   - a target whose state plumb cannot prove clean, where a caller who thought
//     they were removing an empty probe directory should hear why plumb paused.
//
// The refusal names what would be lost and both ways forward. That is the
// decision confirm:true asks the caller to make knowingly; a bare "refused"
// would only teach them to pass confirm:true by reflex.
func (t *Git) checkWorktreeRemove(ctx context.Context, a gitToolArgs) error {
	if len(a.Args) == 0 || a.Args[0] != "remove" || a.Confirm {
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
		return nil
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	if err := refuseOrphanedHead(ctx, target, force); err != nil {
		return err
	}
	dirty := dirtyBasenamesInDir(ctx, target, nil, false)
	if len(dirty) == 0 {
		if !force {
			return nil
		}
		return fmt.Errorf("git worktree remove %s: this worktree is clean, so it needs no --force — "+
			"here --force only overrides what git protects: a LOCKED worktree, or one holding a submodule checkout "+
			"whose directory git would otherwise refuse to remove. Re-run without --force, or with confirm:true to force it deliberately",
			target)
	}
	lost := "that worktree holds " + dirtySummary(dirty)
	discard := textfmt.Plural(len(dirty), "it", "them")
	if force {
		return fmt.Errorf("git worktree remove --force %s: %s, and --force removes it anyway, discarding %s. "+
			"Commit or stash there first, or re-run with confirm:true to discard %s deliberately",
			target, lost, discard, discard)
	}
	return fmt.Errorf("git worktree remove %s: %s, and removing it discards %s. "+
		"Commit or stash there first, or re-run with confirm:true to discard %s deliberately",
		target, lost, discard, discard)
}

// refuseOrphanedHead refuses the removal of a worktree whose detached HEAD holds
// commits nothing else can reach: the directory's reflog is the last thing
// naming them, and it is deleted with the directory.
//
// `--not --branches --tags --remotes` is deliberate: --all would also count the
// HEAD of every worktree in the repository — this one's included — so a commit
// only this worktree points at would look reachable and the guard would never
// fire. `force` does not lift this either; confirm does, in the caller.
func refuseOrphanedHead(ctx context.Context, target string, force bool) error {
	head := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "-C", target, "symbolic-ref", "-q", "HEAD")
	if err := head.Run(); err == nil {
		return nil // on a branch: the branch names whatever it points at
	}
	out, err := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "-C", target,
		"rev-list", "--count", "HEAD", "--not", "--branches", "--tags", "--remotes").Output()
	if err != nil {
		return nil // not a repository plumb can read: git's own failure is the better report
	}
	count, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if convErr != nil || count == 0 {
		return nil
	}
	forced := ""
	if force {
		forced = " --force does not help here:"
	}
	return fmt.Errorf("git worktree remove%s %s: it is on a detached HEAD, and %s reachable from no branch, tag or remote — "+
		"removing it deletes the reflog that is the only thing naming %s. Create a branch there first "+
		"(`git -C %s switch -c <name>`), or re-run with confirm:true to remove it anyway",
		forced, target, textfmt.Plural(count, "1 commit is", fmt.Sprintf("%d commits are", count)),
		textfmt.Plural(count, "it", "them"), target)
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
	summary := fmt.Sprintf("%d uncommitted %s (%s", len(names), textfmt.Plural(len(names), "entry", "entries"),
		strings.Join(shown, ", "))
	if len(names) > len(shown) {
		summary += fmt.Sprintf(", … %d more", len(names)-len(shown))
	}
	return summary + ")"
}
