package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// git_commit_amend.go holds the one guard `git commit --amend` needs (PLAN-493).
// Amending is the natural way to fold a review fix into the commit under review,
// but it REWRITES HEAD: safe while the commit is private, someone else's problem
// once it is published — the push is then rejected and the usual answer is a
// force-push over shared history. plumb can tell the two apart, so it does.

// checkCommitAmend refuses an amend whose HEAD is already reachable from a
// remote-tracking ref, naming the refs that make it published.
//
// The check reads only refs/remotes, so a local-only branch — the worktree and
// branch-per-item flow this tool exists for — never blocks an amend, and a
// repository with no remotes at all never pays for the question.
func checkCommitAmend(ctx context.Context, a gitToolArgs) error {
	if !a.Amend || a.Subcommand != "commit" {
		return nil
	}
	root, err := findGitRoot(a.Repo)
	if err != nil {
		// No repository to resolve against: git's own failure is the better report.
		return nil
	}
	refs := remoteRefsContainingHead(ctx, root)
	if len(refs) == 0 {
		return nil
	}
	shown := refs
	if len(shown) > 3 {
		shown = shown[:3]
	}
	more := ""
	if len(refs) > len(shown) {
		more = fmt.Sprintf(", … %d more", len(refs)-len(shown))
	}
	return fmt.Errorf("git commit --amend: HEAD is already published on %s%s, so amending it rewrites history other clones "+
		"already have — and the push that follows is a force-push. Commit the change as a NEW commit instead; "+
		"amend is for a commit no remote has yet",
		strings.Join(shown, ", "), more)
}

// remoteRefsContainingHead returns the remote-tracking refs that contain HEAD,
// shortest name first as git reports them. `for-each-ref --contains` is the
// read-only way to ask, and --no-optional-locks keeps it from refreshing (and so
// rewriting) the index on the way, exactly as the read tier does.
func remoteRefsContainingHead(ctx context.Context, repoRoot string) []string {
	cmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks,
		"for-each-ref", "--contains", "HEAD", "--format=%(refname:short)", "refs/remotes/")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		// A failure here is not proof of publication, and the amend's own git call
		// will report a real problem; refusing on a broken read would block the case
		// the guard exists to allow.
		return nil
	}
	var refs []string
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			refs = append(refs, line)
		}
	}
	return refs
}
