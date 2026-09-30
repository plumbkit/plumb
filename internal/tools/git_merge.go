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
// neighbours: a state flag anywhere in args lifts the whole call to destructive.
// git expands any unambiguous prefix of a long option, so `--ab` IS --abort and
// is matched as one — an exact match alone would let the abbreviation run the
// reset at the write tier.
func classifyMerge(args []string) gitTier {
	for _, a := range args {
		name, ok := strings.CutPrefix(a, "--")
		if !ok {
			continue
		}
		name, _, _ = strings.Cut(name, "=")
		if isLongPrefix(name, "abort", 2) || isLongPrefix(name, "quit", 2) || isLongPrefix(name, "continue", 3) {
			return tierDestructive
		}
	}
	return tierWrite
}

// mergeValueFlags are merge's short options that take their value as the rest
// of the token or as the next argument, so the text after them is a value to
// skip rather than more flags to inspect.
const mergeValueFlags = "msSX"

// checkMergeArgs refuses the merge flags that would step outside what the tool
// promises of a commit-making operation. git accepts any unambiguous prefix of a
// long option, so each long form is matched by prefix, and bundled short flags
// (`-ne`) are unpacked.
func checkMergeArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return nil
		}
		if err := checkMergeArg(a); err != nil {
			return err
		}
		if a == "-m" || a == "-s" || a == "-X" {
			i++ // the next token is this flag's value, whatever it spells
		}
	}
	return nil
}

func checkMergeArg(a string) error {
	if name, ok := strings.CutPrefix(a, "--"); ok {
		name, _, _ = strings.Cut(name, "=")
		switch {
		case isLongPrefix(name, "no-verify", 4):
			return errors.New("git merge: --no-verify is not permitted — the tool always runs the repository's hooks, " +
				"as it does for commit (pre-merge-commit and commit-msg here)")
		case isLongPrefix(name, "edit", 1):
			return errMergeEditor
		case isLongPrefix(name, "file", 2):
			return errMergeFile
		case isLongPrefix(name, "continue", 3):
			return errMergeContinue
		}
		return nil
	}
	if !strings.HasPrefix(a, "-") || len(a) < 2 {
		return nil
	}
	for _, c := range a[1:] {
		switch {
		case c == 'e':
			return errMergeEditor
		case c == 'F':
			return errMergeFile
		case strings.ContainsRune(mergeValueFlags, c):
			return nil // the rest of the token is this flag's value
		}
	}
	return nil
}

// isLongPrefix reports whether name is an abbreviation git would expand to
// full: a prefix of it at least minLen characters long.
func isLongPrefix(name, full string, minLen int) bool {
	return len(name) >= minLen && strings.HasPrefix(full, name)
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
