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

// withNestedInLibWorktree gives the submodule lib a submodule of its own, inner,
// then a linked worktree x of lib with inner initialised in it. x/inner's git
// directory lives under lib's worktrees/x/modules, unrelated to lib/inner's, so
// only lib's own repository is shared, one level above both files. x's test
// script KILLS a 42→43 mutant of inner/target.txt; lib's passes everything
// (newSubmoduleEnv(t, true)). It returns x.
func (e *submoduleEnv) withNestedInLibWorktree(t *testing.T) string {
	t.Helper()
	innerUp := filepath.Join(filepath.Dir(e.super), "inner-upstream")
	gwWrite(t, innerUp, "target.txt", worktreeTargetOriginal)
	gitInit(t, innerUp)
	lib := filepath.Join(e.super, "lib")
	gwGit(t, lib, "config", "commit.gpgsign", "false")
	gwGit(t, lib, "-c", "protocol.file.allow=always", "submodule", "add", "-q", innerUp, "inner")
	gwGit(t, lib, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "add inner")
	x := filepath.Join(lib, ".claude", "worktrees", "x")
	gwGit(t, lib, "worktree", "add", "-q", "-b", "libx", x)
	gwGit(t, x, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "-q")
	gwWrite(t, x, "test.sh", fmt.Sprintf("echo \"$(/bin/pwd)\" >> %s\n", shellQuote(e.log))+
		"grep -q 43 \"$(dirname \"$0\")/inner/target.txt\" && { echo '--- FAIL: TestInner (0.00s)'; exit 1; }\nexit 0\n")
	gwGit(t, x, "add", "test.sh")
	gwGit(t, x, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "this copy kills")
	return x
}

// TestMutationTest_ANestedSubmoduleInTheSubmodulesWorktreeReRootsThere: from a
// session on lib, a file in x/inner shares no repository with lib at its own level
// and none at the outermost one (the superproject), only lib's repository in
// between: x is lib's worktree. The commands move to x.
func TestMutationTest_ANestedSubmoduleInTheSubmodulesWorktreeReRootsThere(t *testing.T) {
	e := newSubmoduleEnv(t, true)
	x := e.withNestedInLibWorktree(t)

	out, err := executeMutants(t, e.tool(filepath.Join(e.super, "lib"), "", ""), filepath.Join(x, "inner", "target.txt"))
	if err != nil {
		t.Fatalf("a nested submodule of lib's worktree must re-root into that worktree: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") || !strings.Contains(out, "re-rooted") {
		t.Errorf("x's test kills the mutant after a re-root; got:\n%s", out)
	}
	e.requireAllRanIn(t, x)
	requireContent(t, filepath.Join(x, "inner", "target.txt"), worktreeTargetOriginal)
}

// TestMutationTest_ASessionOnTheNestedSubmoduleReRootsToItsWorktreeCopy: the same
// from a session on lib/inner; the same relative directory, inner, is taken in x.
func TestMutationTest_ASessionOnTheNestedSubmoduleReRootsToItsWorktreeCopy(t *testing.T) {
	e := newSubmoduleEnv(t, true)
	x := e.withNestedInLibWorktree(t)

	out, err := executeMutants(t, e.tool(filepath.Join(e.super, "lib", "inner"), "", "../"), filepath.Join(x, "inner", "target.txt"))
	if err != nil {
		t.Fatalf("from lib/inner, the mutant in x/inner must re-root to x/inner: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") || !strings.Contains(out, "re-rooted") {
		t.Errorf("x's test kills the mutant after a re-root; got:\n%s", out)
	}
	e.requireAllRanIn(t, filepath.Join(x, "inner"))
}
