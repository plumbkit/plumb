package tools

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/plumbkit/plumb/internal/textfmt"
)

// git_merge.go holds what is particular to the merge subcommand (#530): which
// forms are writes and which are destructive, the flags refused because they
// escape the tool's contract, and how a merge that stops on conflicts is
// reported.
//
// Merging the base branch into a work branch is the non-rewriting way to update
// it, so an ordinary merge sits in the WRITE tier beside commit: it adds a
// commit (or fast-forwards) and runs the same kind of hooks. The state flags are
// treated as rebase's and cherry-pick's are: --abort resets the working tree,
// discarding any conflict resolution, and --quit strands a half-done merge, so
// both are DESTRUCTIVE. --continue is refused outright rather than tiered: it
// runs `git commit` with no message, which opens an editor the tool cannot drive,
// and the tool's own commit concludes a merge just as well.

// classifyMerge is the merge arm of classifyGit. Safe-biased like its
// neighbours: a state flag anywhere in args lifts the whole call to destructive,
// read as git reads it (git_options.go) — so `--ab` IS --abort.
func classifyMerge(args []string) gitTier {
	if mergeGrammar.has(args, false, "", "abort", "quit", "continue") {
		return tierDestructive
	}
	return tierWrite
}

// checkMergeArgs refuses the merge flags that would step outside what the tool
// promises of a commit-making operation. It reads args exactly as git's option
// parser will (mergeGrammar.scan), because a refused flag hidden where git sees
// a VALUE is harmless, and one hidden where the check sees a value but git sees
// an option is a bypass. `--` does not end the check: it may be another
// option's value, and after a genuine `--` nothing git takes can start with "-"
// (no ref name can), so checking on costs nothing.
func checkMergeArgs(args []string) error {
	var err error
	mergeGrammar.scan(args, false, func(o gitOption) bool {
		switch {
		case o.is("", "no-verify"):
			err = errors.New("git merge: --no-verify is not permitted — the tool always runs the repository's hooks, " +
				"as it does for commit (pre-merge-commit and commit-msg here)")
		case o.is("e", "edit"):
			err = errMergeEditor
		case o.is("F", "file"):
			err = errMergeFile
		case o.is("", "continue"):
			err = errMergeContinue
		}
		return err == nil
	})
	return err
}

var (
	errMergeEditor = errors.New("git merge: -e/--edit is not permitted — it opens an editor the tool cannot drive. " +
		"Pass the message with -m, or --no-edit to accept git's default")
	errMergeFile = errors.New("git merge: -F/--file is not permitted — it reads the message from a file outside the tool's " +
		"path checks. Pass the message with -m")
	errMergeContinue = errors.New("git merge: --continue is not permitted — it runs `git commit` with no message, which opens an editor " +
		"the tool cannot drive. Conclude the merge with subcommand \"commit\" and a message instead: stage the resolved files with " +
		"subcommand \"add\" first, and the commit records the two-parent merge and runs the hooks")
)

// mergeConflictHeadline replaces a failed merge's generic headline when git
// stopped on conflicts, naming the conflicted files and the two ways on. It is
// "" for any other failure — a refusing hook, --ff-only on a diverged branch,
// an unknown ref — which the ordinary report already describes.
//
// Conflicts are read from the index (`diff --diff-filter=U`) rather than parsed
// from git's CONFLICT lines, whose wording varies by conflict kind and locale.
func mergeConflictHeadline(repoRoot, sub string, runErr error) string {
	if sub != "merge" || !isExitCode(runErr, 1) {
		return ""
	}
	files := unmergedPaths(repoRoot)
	if len(files) == 0 {
		return ""
	}
	return fmt.Sprintf("git merge: stopped with conflicts in %d %s — the repository is left in git's merging state "+
		"(MERGE_HEAD) for you to resolve:\n  %s\n"+
		"Resolve each file, stage it with subcommand \"add\" (files), then conclude the merge with subcommand \"commit\" "+
		"(message), which records the two-parent merge commit and runs the hooks. To abandon the merge instead: "+
		"subcommand \"merge\", args [\"--abort\"], confirm: true (destructive tier).",
		len(files), textfmt.Plural(len(files), "file", "files"), strings.Join(files, "\n  "))
}

// unmergedPaths lists the index's unmerged paths, or nil when they cannot be
// read — a failure to enumerate them must not hide the ordinary report.
func unmergedPaths(repoRoot string) []string {
	cmd := exec.Command("git", gitNoOptionalLocks, "diff", "--name-only", "--diff-filter=U")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}
