package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mutationtest_twin_test.go: layouts where the commands' tree holds ANOTHER copy
// of a mutated file although the file's own tree looks like an unrelated
// repository's main work-tree. "Stay put" there tests the commands' copy, and
// every mutant reads SURVIVED from the wrong tree, the one outcome this feature
// must never produce. Each case must move to the file's copy, or be refused.

// TestMutationTest_FromASubmodulesWorktreeTheCheckoutsNestedFileMovesThere: from
// lib's worktree x, a file in lib/inner (the checkout's nested submodule) is x's
// inner/ at lib's level; the commands move to lib. lib's test passes everything,
// so the honest verdict is SURVIVED, reached in lib.
func TestMutationTest_FromASubmodulesWorktreeTheCheckoutsNestedFileMovesThere(t *testing.T) {
	e := newSubmoduleEnv(t, true)
	x := e.withNestedInLibWorktree(t)
	lib := filepath.Join(e.super, "lib")

	out, err := executeMutants(t, e.tool(x, "", ""), filepath.Join(lib, "inner", "target.txt"))
	if err != nil {
		t.Fatalf("a file in lib/inner must move the commands from x to lib: %v", err)
	}
	if !strings.Contains(out, "re-rooted") || !strings.Contains(out, "[1] SURVIVED") {
		t.Errorf("the run must move to lib, whose passing test reads SURVIVED; got:\n%s", out)
	}
	e.requireAllRanIn(t, lib)
}

// TestMutationTest_MutantsInTheSuperprojectAndTheSubmoduleFromItsWorktreeAreRefused:
// from lib's worktree x, S/other.txt shares no repository with x, so it may run
// where the commands are; S/lib/target.txt alone would move them to lib. Together
// they need two places, and running both in x would test x's copy of lib's file.
func TestMutationTest_MutantsInTheSuperprojectAndTheSubmoduleFromItsWorktreeAreRefused(t *testing.T) {
	e := newSubmoduleEnv(t, true)
	x := e.withLibWorktree(t)
	gwWrite(t, e.super, "other.txt", worktreeTargetOriginal)
	gwGit(t, e.super, "add", "other.txt")
	gwGit(t, e.super, "commit", "-q", "-m", "other")

	raw, err := json.Marshal(map[string]any{"mutants": []mutantJSON{
		{"file_path": filepath.Join(e.super, "other.txt"), "old_string": "42", "new_string": "43"},
		{"file_path": filepath.Join(e.super, "lib", "target.txt"), "old_string": "42", "new_string": "43"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.tool(x, "", "").Execute(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "more than one git work-tree") {
		t.Fatalf("mutants that need x and lib at once must be refused; got %v", err)
	}
	if b, rerr := os.ReadFile(e.log); rerr == nil && len(b) > 0 {
		t.Errorf("a refusal must come before anything runs; ran:\n%s", b)
	}
}

// TestMutationTest_AWorktreeOfAWorktreesSubmoduleMovesToTheCheckoutsCopy: xw is a
// worktree of lib as checked out inside the superproject's worktree wt, so its git
// directory lives under wt's, not under the main checkout's lib. The main
// checkout's lib is still a copy of the same submodule; the commands move there.
func TestMutationTest_AWorktreeOfAWorktreesSubmoduleMovesToTheCheckoutsCopy(t *testing.T) {
	e := newSubmoduleEnv(t, false) // the MAIN checkout's lib kills; wt/lib's passes
	wtLib := filepath.Join(e.wt, "lib")
	xw := filepath.Join(wtLib, ".claude", "worktrees", "xw")
	gwGit(t, wtLib, "worktree", "add", "-q", "-b", "xw", xw)
	lib := filepath.Join(e.super, "lib")

	out, err := executeMutants(t, e.tool(xw, "", ""), filepath.Join(lib, "target.txt"))
	if err != nil {
		t.Fatalf("the main checkout's lib is another copy of xw's submodule; the run must move there: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") || !strings.Contains(out, "re-rooted") {
		t.Errorf("the main checkout's lib test kills the mutant after a re-root; got:\n%s", out)
	}
	e.requireAllRanIn(t, lib)
}

func TestRepoKey(t *testing.T) {
	for in, want := range map[string]string{
		"/s/.git":                          "/s/.git",
		"/s/.git/modules/lib":              "/s/.git/modules/lib",
		"/s/.git/worktrees/wt/modules/lib": "/s/.git/modules/lib",
		"/s/.git/worktrees/wt/modules/lib/modules/inner":        "/s/.git/modules/lib/modules/inner",
		"/s/.git/modules/lib/worktrees/x/modules/inner":         "/s/.git/modules/lib/modules/inner",
		"/s/.git/worktrees/w/modules/lib/worktrees/x/modules/i": "/s/.git/modules/lib/modules/i",
		"/s/.git/worktrees/wt":                                  "/s/.git/worktrees/wt", // a worktree's own git dir is not a submodule's
		"/home/worktrees/a/modules/r/.git":                      "/home/worktrees/a/modules/r/.git",
		"/store/P.git/worktrees/w/modules/lib":                  "/store/P.git/modules/lib", // --separate-git-dir superproject
		"/srv/bare.git":                                         "/srv/bare.git",
	} {
		if got := repoKey(filepath.FromSlash(in)); got != filepath.FromSlash(want) {
			t.Errorf("repoKey(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestMutationTest_FromANestedSubmoduleTheCommandsClimbToTheShared: from a session
// on lib/inner, a file at lib's level in lib's worktree x shares a repository with
// the commands only one level up their chain (lib's). The commands move to the same
// relative directory, inner/, in x, rather than being refused as stranded.
func TestMutationTest_FromANestedSubmoduleTheCommandsClimbToTheShared(t *testing.T) {
	e := newSubmoduleEnv(t, true)
	x := e.withNestedInLibWorktree(t)

	out, err := executeMutants(t, e.tool(filepath.Join(e.super, "lib", "inner"), "", "../"), filepath.Join(x, "target.txt"))
	if err != nil {
		t.Fatalf("a file in lib's worktree must move the commands from lib/inner to x/inner: %v", err)
	}
	if !strings.Contains(out, "re-rooted") {
		t.Errorf("the run must be re-rooted into x; got:\n%s", out)
	}
	e.requireAllRanIn(t, filepath.Join(x, "inner"))
}
