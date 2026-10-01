package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// detachedGitRepo returns a repository with one commit whose HEAD is detached
// at it — the standard review-worktree setup — and the commit's short sha.
func detachedGitRepo(t *testing.T) (ws, sha string) {
	t.Helper()
	ws = briefGitInit(t)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", ws, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("commit", "--allow-empty", "-q", "-m", "init")
	sha = run("rev-parse", "--short", "HEAD")
	run("checkout", "-q", "--detach", "HEAD")
	return ws, sha
}

// TestSessionStart_DetachedHEADKeepsGitPolicy is issue #546 item 2: on a
// detached HEAD both packets still name where HEAD is and still carry the git
// policy. Keying both on a branch name dropped them for every review worktree.
func TestSessionStart_DetachedHEADKeepsGitPolicy(t *testing.T) {
	ws, sha := detachedGitRepo(t)
	policy := GitPolicy{AllowWrites: true}
	for detail, wantPolicy := range map[string]string{
		"brief": "Git:      writes on, destructive off, push off",
		"full":  "## Git (via the `git` tool — live policy)",
	} {
		t.Run(detail, func(t *testing.T) {
			tool := NewSessionStart(func(context.Context) string { return ws }, &stubDiagnostics{}, nil, nil,
				func() string { return "" }, func() GitPolicy { return policy })
			out, err := tool.Execute(context.Background(), json.RawMessage(`{"detail":"`+detail+`"}`))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if want := "Branch:   detached at " + sha + "\n"; !strings.Contains(out, want) {
				t.Errorf("want %q on a detached HEAD:\n%s", want, out)
			}
			if !strings.Contains(out, wantPolicy) {
				t.Errorf("the git policy must survive a detached HEAD (want %q):\n%s", wantPolicy, out)
			}
		})
	}
}

// TestGitHeadLabel pins the three answers: the branch on a branch, the short
// sha on a detached HEAD, and nothing outside a repository — where the git
// policy section stays hidden.
func TestGitHeadLabel(t *testing.T) {
	onBranch := briefGitInit(t)
	if got := gitHeadLabel(onBranch); got == "" || strings.HasPrefix(got, "detached") {
		t.Errorf("unborn branch: got %q, want the branch name", got)
	}
	detached, sha := detachedGitRepo(t)
	if got, want := gitHeadLabel(detached), "detached at "+sha; got != want {
		t.Errorf("detached HEAD: got %q, want %q", got, want)
	}
	// The test's temp dir may sit inside a checkout (GOTMPDIR=.testcache), so
	// stop git's discovery at it.
	bare := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(bare))
	if got := gitHeadLabel(bare); got != "" {
		t.Errorf("not a repository: got %q, want \"\"", got)
	}
}
