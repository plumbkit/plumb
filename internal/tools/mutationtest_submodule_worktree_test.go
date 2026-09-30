package tools

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// mutationtest_submodule_worktree_test.go covers the plumb-ops layout: the session
// is on a SUBMODULE's main checkout (super/lib), and the mutated file is in a linked
// worktree of that same submodule (super/lib/.claude/worktrees/libwt). git names one
// repository for both, but only the checkout has a superproject, so climbing to the
// outermost superproject made the two look unrelated and the run was refused as
// "a different repository".

// withLibWorktree adds a linked worktree of the submodule e.super/lib whose own test
// script KILLS the 42→43 mutant, while the checkout's (newSubmoduleEnv(t, true))
// passes everything. It returns the worktree's root.
func (e *submoduleEnv) withLibWorktree(t *testing.T) string {
	t.Helper()
	lib := filepath.Join(e.super, "lib")
	libwt := filepath.Join(lib, ".claude", "worktrees", "libwt")
	gwGit(t, lib, "worktree", "add", "-q", "-b", "libwt", libwt)
	gwWrite(t, libwt, "test.sh", fmt.Sprintf("echo \"$(/bin/pwd)\" >> %s\n", shellQuote(e.log))+
		"grep -q 43 \"$(dirname \"$0\")/target.txt\" && { echo '--- FAIL: TestAnswer (0.00s)'; exit 1; }\nexit 0\n")
	gwGit(t, libwt, "config", "commit.gpgsign", "false")
	gwGit(t, libwt, "add", "-A")
	gwGit(t, libwt, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "this copy kills")
	return libwt
}

func TestMutationTest_ASubmoduleCheckoutSessionReRootsIntoTheSubmodulesOwnWorktree(t *testing.T) {
	e := newSubmoduleEnv(t, true) // the checkout's copy passes everything
	libwt := e.withLibWorktree(t)

	out, err := executeMutants(t, e.tool(filepath.Join(e.super, "lib"), "", ""), filepath.Join(libwt, "target.txt"))
	if err != nil {
		t.Fatalf("a file in the submodule's own worktree must re-root there, not be refused: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") || !strings.Contains(out, "re-rooted") {
		t.Errorf("the worktree's copy kills the mutant after a re-root; got:\n%s", out)
	}
	e.requireAllRanIn(t, libwt)
	requireContent(t, filepath.Join(libwt, "target.txt"), worktreeTargetOriginal)
}

// TestMutationTest_ASubmoduleWorktreeSessionReRootsToTheSubmoduleCheckout is the
// other direction: the session is on the submodule's worktree and the file is in
// the checkout, whose passing test honestly reports SURVIVED.
func TestMutationTest_ASubmoduleWorktreeSessionReRootsToTheSubmoduleCheckout(t *testing.T) {
	e := newSubmoduleEnv(t, true)
	libwt := e.withLibWorktree(t)
	lib := filepath.Join(e.super, "lib")

	out, err := executeMutants(t, e.tool(libwt, "", ""), filepath.Join(lib, "target.txt"))
	if err != nil {
		t.Fatalf("a file in the submodule's checkout must re-root there from its worktree: %v", err)
	}
	if !strings.Contains(out, "[1] SURVIVED") || !strings.Contains(out, "re-rooted") {
		t.Errorf("the checkout's passing test must be the one that ran; got:\n%s", out)
	}
	e.requireAllRanIn(t, lib)
}
