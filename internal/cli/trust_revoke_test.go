package cli

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// trust_revoke_test.go covers `plumb trust --revoke` and the surfaces that name
// a shared grant's source (#540 review, N2). A linked worktree shares its
// repository's grant and has no record of its own, so a revoke run in the
// worktree removes nothing; saying only "revoked" there would leave the project's
// commands running under a message claiming otherwise.

func worktreeOfTrustedRepo(t *testing.T) (main, wt string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main = filepath.Join(dir, "main")
	writeProjectConfig(t, main, "[git]\nallow_push = true\n")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false", "add", "-A"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = main
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	wt = filepath.Join(main, ".claude", "worktrees", "wt")
	cmd := exec.Command("git", "worktree", "add", "-q", "-b", "feature", wt, "main")
	cmd.Dir = main
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	grantExecTrust(t, main)
	return main, wt
}

func revokeOutput(t *testing.T, root string) string {
	t.Helper()
	cmds, err := config.ProjectTaskCommands(root)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := config.ProjectPolicySpecFor(root)
	if err != nil {
		t.Fatal(err)
	}
	return captureStdout(t, func() {
		if err := revokeTrust(root, cmds, spec); err != nil {
			t.Fatalf("revokeTrust: %v", err)
		}
	})
}

func TestTrustRevoke_InAWorktreeNamesTheSharedGrant(t *testing.T) {
	main, wt := worktreeOfTrustedRepo(t)

	out := revokeOutput(t, wt)
	for _, want := range []string{"STILL trusted", main, "plumb trust --revoke " + main} {
		if !strings.Contains(out, want) {
			t.Errorf("revoke in a worktree sharing %s does not say %q:\n%s", main, want, out)
		}
	}

	// Revoked at its source, the grant ends for the worktree too, and a second
	// revoke there has nothing left to report.
	_ = revokeOutput(t, main)
	if out := revokeOutput(t, wt); strings.Contains(out, "STILL trusted") {
		t.Errorf("revoke reports a shared grant after the source was revoked:\n%s", out)
	}
}

// TestProjectGitStatus_NamesTheSharedGrant: the session's snapshot carries the
// source, so session_start can say where to revoke it.
func TestProjectGitStatus_NamesTheSharedGrant(t *testing.T) {
	main, wt := worktreeOfTrustedRepo(t)
	st := projectGitSession(t, wt).projectGitStatus()
	if !st.Trusted || st.InheritedFrom != main {
		t.Errorf("worktree snapshot = trusted %v, inherited from %q; want trusted, from %q", st.Trusted, st.InheritedFrom, main)
	}
	if st := projectGitSession(t, main).projectGitStatus(); st.InheritedFrom != "" {
		t.Errorf("the main checkout's own grant reports a source %q", st.InheritedFrom)
	}
}
