package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mutationtest_submodule_test.go covers submodules and linked worktrees together —
// the layout plumb-ops itself uses: a superproject whose worktree has the submodule
// checked out inside it. The submodule's own git directory then looks like an
// unrelated repository's main work-tree, and a first fix that let such files "run in
// place" tested the WORKTREE's submodule mutant with the MAIN checkout's copy:
// SURVIVED from the wrong tree. A file in a submodule is placed by its outermost
// superproject instead, so it re-roots like any other file of that work-tree.

// submoduleEnv is a superproject with a submodule `lib`, a linked worktree of the
// superproject with the submodule initialised in it, and scripts in each copy of
// lib that log where they ran. Exactly one copy's test kills the 42→43 mutant.
type submoduleEnv struct {
	super, wt, log string
}

func newSubmoduleEnv(t *testing.T, worktreeKills bool) *submoduleEnv {
	t.Helper()
	requireGit(t)
	unsetEnvForTest(t, "GOWORK")
	root := evalTempDir(t)
	e := &submoduleEnv{log: filepath.Join(t.TempDir(), "ran.log")}
	scripts := func(dir string, kills bool) {
		logLine := fmt.Sprintf("echo \"$(/bin/pwd)\" >> %s\n", shellQuote(e.log))
		test := logLine
		if kills {
			test += "grep -q 43 \"$(dirname \"$0\")/target.txt\" && { echo '--- FAIL: TestAnswer (0.00s)'; exit 1; }\n"
		}
		gwWrite(t, dir, "compile.sh", logLine+"exit 0\n")
		gwWrite(t, dir, "test.sh", test+"exit 0\n")
	}

	up := filepath.Join(root, "lib-upstream")
	gwWrite(t, up, "target.txt", worktreeTargetOriginal)
	scripts(up, !worktreeKills)
	gitInit(t, up)

	e.super = filepath.Join(root, "super")
	gwWrite(t, e.super, "README", "x\n")
	gitInit(t, e.super)
	gwGit(t, e.super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", up, "lib")
	gwGit(t, e.super, "commit", "-q", "-m", "add lib")

	e.wt = filepath.Join(e.super, ".claude", "worktrees", "wt")
	gwGit(t, e.super, "worktree", "add", "-q", "-b", "wt", e.wt)
	gwGit(t, e.wt, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "-q")
	wtLib := filepath.Join(e.wt, "lib")
	gwGit(t, wtLib, "config", "commit.gpgsign", "false")
	scripts(wtLib, worktreeKills)
	gwGit(t, wtLib, "add", "-A")
	gwGit(t, wtLib, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "this copy's scripts")
	return e
}

// tool opens mutation_test with workspace as the session's root and the commands
// `sh <prefix>compile.sh` / `sh <prefix>test.sh` run from workdir ("" = the root).
func (e *submoduleEnv) tool(workspace, workdir, prefix string) *MutationTest {
	return NewMutationTest(WriteDeps{WorkspaceFn: func(context.Context) string { return workspace }},
		func(_ context.Context, req TaskRequest) (TaskCommand, error) {
			slot := req.Slot
			script := prefix + "compile.sh"
			if slot == "test" {
				script = prefix + "test.sh"
			}
			return TaskCommand{Slot: slot, Steps: [][]string{{"/bin/sh", script}}, Provenance: "default", WorkingDir: workdir}, nil
		})
}

func (e *submoduleEnv) ranIn(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(e.log)
	if err != nil {
		t.Fatalf("no script ran: %v", err)
	}
	return strings.Fields(string(b))
}

func (e *submoduleEnv) requireAllRanIn(t *testing.T, want string) {
	t.Helper()
	for _, d := range e.ranIn(t) {
		if d != want {
			t.Errorf("a command ran in %s, want every one in %s", d, want)
		}
	}
}

func TestMutationTest_ASubmoduleInsideAWorktreeReRootsIntoThatWorktree(t *testing.T) {
	e := newSubmoduleEnv(t, true) // the WORKTREE's copy kills
	out, err := executeMutants(t, e.tool(e.super, "", "lib/"), filepath.Join(e.wt, "lib", "target.txt"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") || !strings.Contains(out, "re-rooted") {
		t.Errorf("the worktree's submodule copy kills the mutant, and the report must say the run was re-rooted; got:\n%s", out)
	}
	e.requireAllRanIn(t, e.wt)
	requireContent(t, filepath.Join(e.wt, "lib", "target.txt"), worktreeTargetOriginal)
}

func TestMutationTest_AMainSubmoduleFromAWorktreeSessionReRootsToTheMainCheckout(t *testing.T) {
	e := newSubmoduleEnv(t, false) // the MAIN checkout's copy kills
	out, err := executeMutants(t, e.tool(e.wt, "", "lib/"), filepath.Join(e.super, "lib", "target.txt"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") {
		t.Errorf("the main checkout's submodule copy kills the mutant; got:\n%s", out)
	}
	e.requireAllRanIn(t, e.super)
}

// TestMutationTest_ASubmoduleOfTheRunTreeStaysPut is the control: the submodule is
// the one inside the tree the commands already run in, so nothing moves — and the
// main copy's test, which passes everything, honestly reports SURVIVED.
func TestMutationTest_ASubmoduleOfTheRunTreeStaysPut(t *testing.T) {
	e := newSubmoduleEnv(t, true)
	out, err := executeMutants(t, e.tool(e.super, "", "lib/"), filepath.Join(e.super, "lib", "target.txt"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out, "re-rooted") || !strings.Contains(out, "[1] SURVIVED") {
		t.Errorf("a submodule of the run tree must run in place; got:\n%s", out)
	}
	e.requireAllRanIn(t, e.super)
}

// TestMutationTest_AWorkingDirInsideTheSubmoduleKeepsItsPlace: working_dir is the
// submodule itself; the same relative directory is taken in the worktree.
func TestMutationTest_AWorkingDirInsideTheSubmoduleKeepsItsPlace(t *testing.T) {
	e := newSubmoduleEnv(t, true)
	out, err := executeMutants(t, e.tool(e.super, filepath.Join(e.super, "lib"), ""), filepath.Join(e.wt, "lib", "target.txt"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") {
		t.Errorf("want the worktree submodule's kill; got:\n%s", out)
	}
	e.requireAllRanIn(t, filepath.Join(e.wt, "lib"))
}

// TestPlacement_ClimbsToTheOutermostSuperproject pins the placement itself, and the
// on-disk fallback's reading of a worktree submodule's git directory.
func TestPlacement_ClimbsToTheOutermostSuperproject(t *testing.T) {
	e := newSubmoduleEnv(t, true)
	ctx := context.Background()
	g := gitProbes{}
	p := g.placement(ctx, filepath.Join(e.wt, "lib"))
	if p.place != placeTree || p.tree.top != e.wt || p.tree.prefix != "lib/" || !p.tree.linked {
		t.Errorf("placement(wt/lib) = %+v, want the linked worktree %s with prefix lib/", p, e.wt)
	}
	if own := g.of(ctx, filepath.Join(e.wt, "lib")); own.tree.top != filepath.Join(e.wt, "lib") || own.tree.super != e.wt {
		t.Errorf("the memo must keep git's own answer for the submodule; got %+v", own)
	}
	if d := diskProbe(filepath.Join(e.wt, "lib"), "why"); d.place != placeTree || !d.tree.linked {
		t.Errorf("on disk, a submodule inside a linked worktree must read as linked; got %+v", d)
	}
	if d := diskProbe(filepath.Join(e.super, "lib"), "why"); d.place != placeTree || d.tree.linked {
		t.Errorf("on disk, the main checkout's submodule is not linked; got %+v", d)
	}
}

func TestInWorktreeModules(t *testing.T) {
	for dir, want := range map[string]bool{
		"/r/.git/worktrees/wt/modules/lib":             true,
		"/r/.git/worktrees/wt/modules/lib/modules/sub": true,
		"/r/.git/modules/lib":                          false,
		"/r/.git/worktrees/wt":                         false,
		"/r/.git/modules/worktrees":                    false,
	} {
		if got := inWorktreeModules(dir); got != want {
			t.Errorf("inWorktreeModules(%s) = %v, want %v", dir, got, want)
		}
	}
}

// TestRerootCommand_AnUnconfirmedDestinationNamesGitsReason: when git answers for
// the trees but not for the destination directory, the refusal must carry git's
// reason, not guess at a symlink.
func TestRerootCommand_AnUnconfirmedDestinationNamesGitsReason(t *testing.T) {
	root := evalTempDir(t)
	dest := filepath.Join(root, "dest")
	writeTree(t, dest, map[string]string{"sub/x": "x"})
	moved := filepath.Join(dest, "sub")
	probes := gitProbes{moved: {place: placeTree, approx: true, reason: "detected dubious ownership", tree: gitTree{top: dest}}}
	_, err := rerootCommand(context.Background(), probes.of, TaskCommand{Steps: [][]string{{"true"}}}, filepath.Join(root, "from", "sub"),
		gitTree{top: filepath.Join(root, "from"), prefix: "sub/"}, gitTree{top: dest}, "x")
	if err == nil || !strings.Contains(err.Error(), "could not confirm") || !strings.Contains(err.Error(), "dubious ownership") ||
		strings.Contains(err.Error(), "symlink") {
		t.Fatalf("want a refusal naming git's reason, not a symlink guess; got %v", err)
	}
}

// TestProbeGitDir_InsideAGitDirectoryKeepsItsCommonDir: asked from inside a
// submodule's git directory, git names the submodule's work-tree as the top, an
// empty prefix, and "." for both git directories. "." is relative to the git
// directory, not to top plus prefix, so joining it there would name the work-tree
// as the common directory.
func TestProbeGitDir_InsideAGitDirectoryKeepsItsCommonDir(t *testing.T) {
	e := newSubmoduleEnv(t, true)
	modDir := filepath.Join(e.super, ".git", "modules", "lib")
	p := probeGitDir(context.Background(), modDir)
	if p.place != placeTree {
		t.Skipf("this git does not describe a work-tree from inside a git directory: %+v", p)
	}
	if p.tree.common != modDir || p.tree.linked {
		t.Errorf("from inside %s: got %+v, want that directory as the common dir, not linked", modDir, p.tree)
	}
}
