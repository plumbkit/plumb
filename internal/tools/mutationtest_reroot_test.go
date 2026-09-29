package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mutationtest_reroot_test.go holds the regressions found reviewing the first
// version of re-rooting (PLAN-442): each test is one way that version tested the
// wrong tree, refused a valid run, or said something false. They share the
// worktreeEnv fixture in mutationtest_root_test.go, whose two trees DISAGREE on
// purpose, so a run in the wrong tree cannot produce the right verdict by accident.

// --- GOWORK is decided where every task command runs, not only when moved ----

// TestMutationTest_ASessionPinnedToTheWorktreeGetsGOWORKOff: nothing is re-rooted
// when the session is already IN the worktree, and the first version only decided
// GOWORK for moved commands — so this, the plainer case, still hit go's refusal
// under an enclosing go.work and was blamed on the build.
func TestMutationTest_ASessionPinnedToTheWorktreeGetsGOWORKOff(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false) // the worktree's test kills
	e.underGoWork(t)
	e.reopenAt(e.wt)

	out, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") {
		t.Errorf("the worktree's test must kill the mutant; got:\n%s", out)
	}
	e.requireAllRanIn(t, e.wt)
	for _, got := range e.ranGOWORK(t) {
		if got != "off" {
			t.Errorf("a command in the shadowed worktree saw GOWORK=%q, want \"off\"", got)
		}
	}
	if !strings.Contains(out, "GOWORK=off") || strings.Contains(out, "re-rooted") {
		t.Errorf("the report must say GOWORK=off and must not claim a re-root; got:\n%s", out)
	}
}

// TestMutationTest_AReRootIntoAListedWorktreeKeepsItsGoWork is the control the
// first version lacked: an implementation that set GOWORK=off on EVERY re-root
// passed its whole suite. Here the go.work lists the worktree, so the moved
// commands must keep the workspace.
func TestMutationTest_AReRootIntoAListedWorktreeKeepsItsGoWork(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	e.underGoWork(t)
	gwWrite(t, filepath.Dir(e.root), "go.work",
		"go 1.21\n\nuse ./"+filepath.Base(e.root)+"/.claude/worktrees/wt\n")

	out, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	e.requireAllRanIn(t, e.wt)
	for _, got := range e.ranGOWORK(t) {
		if got != "UNSET" {
			t.Errorf("a command re-rooted into a LISTED worktree saw GOWORK=%q, want it untouched", got)
		}
	}
	if strings.Contains(out, "GOWORK=off") {
		t.Errorf("nothing was switched off, so the report must not say so; got:\n%s", out)
	}
}

// TestBaseline_SaysGOWORKWasSwitchedOff: a red baseline under GOWORK=off may be red
// BECAUSE of it (a module only the workspace provides), so the refusal says so
// instead of only "Fix the build first".
func TestBaseline_SaysGOWORKWasSwitchedOff(t *testing.T) {
	e := newWorktreeEnv(t, "", false, true) // the worktree's compile gate is red
	e.underGoWork(t)

	_, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err == nil {
		t.Fatal("a red compile gate must refuse the run")
	}
	for _, want := range []string{"GOWORK=off", filepath.Join(filepath.Dir(e.root), "go.work")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must mention %q; got:\n%v", want, err)
		}
	}
}

// --- the directory the commands move to is git's, not a spelling ------------

// TestMutationTest_AWorkingDirThroughAnInRepoSymlinkReRoots: working_dir reaches
// the module through a link in the repository. git answers relative to the
// PHYSICAL directory, and the first version joined that answer onto the link's
// spelling — naming a different "repository" and refusing a valid run.
func TestMutationTest_AWorkingDirThroughAnInRepoSymlinkReRoots(t *testing.T) {
	e := newWorktreeEnv(t, filepath.Join("src", "go", "mod"), false, false)
	link := filepath.Join(e.root, "gomod")
	if err := os.Symlink(filepath.Join("src", "go", "mod"), link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	e.workdir = link

	out, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err != nil {
		t.Fatalf("a working_dir reached through an in-repo symlink must still re-root: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") {
		t.Errorf("the worktree's test must kill the mutant; got:\n%s", out)
	}
	e.requireAllRanIn(t, filepath.Join(e.wt, "src", "go", "mod"))
}

// TestMutationTest_ANestedCloneReachedThroughASymlinkIsNotReRooted: a mutant named
// through an in-repo link INTO a nested clone. The first version joined the clone's
// relative "../.git" onto the link's spelling, got the OUTER repository's common
// directory, took the clone for a worktree of it and ran the workspace's trusted
// commands inside the clone. It is another repository's main work-tree: the
// commands stay where they are.
func TestMutationTest_ANestedCloneReachedThroughASymlinkIsNotReRooted(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	clone := filepath.Join(e.root, "third_party", "b")
	gwWrite(t, filepath.Join(clone, "pkg"), "target.txt", worktreeTargetOriginal)
	gitInit(t, clone)
	link := filepath.Join(e.root, "blink")
	if err := os.Symlink(filepath.Join("third_party", "b", "pkg"), link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	out, err := e.run(t, e.mutant(filepath.Join(link, "target.txt"), "42", "43"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out, "re-rooted") {
		t.Errorf("a nested clone is another repository; nothing may be re-rooted into it; got:\n%s", out)
	}
	e.requireAllRanIn(t, e.root)
}

// TestMutationTest_ASymlinkedWorkingDirInTheWorktreeIsRefused: working_dir is a
// real directory in the main checkout, but on the worktree's branch the same name
// is a LINK — back into the main checkout, or out of the workspace. The first
// version checked only that the path was a directory, then ran there and reported
// "ran in: <worktree>/module" while every script ran wherever the link led.
func TestMutationTest_ASymlinkedWorkingDirInTheWorktreeIsRefused(t *testing.T) {
	for _, into := range []string{"the main checkout", "outside the workspace"} {
		t.Run(into, func(t *testing.T) {
			e := newWorktreeEnv(t, "pkg", false, false)
			module := filepath.Join(e.root, "module")
			e.writeScripts(t, module, false, false)
			e.workdir = module
			target := module
			if into == "outside the workspace" {
				target = filepath.Join(evalTempDir(t), "elsewhere")
				e.writeScripts(t, target, false, false)
			}
			if err := os.Symlink(target, filepath.Join(e.wt, "module")); err != nil {
				t.Skipf("symlink: %v", err)
			}

			_, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
			if err == nil {
				t.Fatal("a working_dir that is a link elsewhere in the destination work-tree must be refused")
			}
			if !strings.Contains(err.Error(), "resolves to") {
				t.Errorf("the refusal must say where the directory really leads; got:\n%v", err)
			}
			e.requireNothingRan(t)
		})
	}
}

// TestMutationTest_ASiblingWorktreesScriptIsRefused: the command names a script in
// ANOTHER worktree whose name merely extends the destination's (wt-old beside wt).
// Substring matching read it as "in the destination", let it through, and the run
// executed wt-old's tests: SURVIVED from the wrong tree.
func TestMutationTest_ASiblingWorktreesScriptIsRefused(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	old := filepath.Join(e.root, ".claude", "worktrees", "wt-old")
	gwGit(t, e.root, "worktree", "add", "-q", "-b", "old", old)
	e.testArgv = []string{"/bin/sh", filepath.Join(old, "test.sh")}

	_, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err == nil || !strings.Contains(err.Error(), "argument") || !strings.Contains(err.Error(), old) {
		t.Fatalf("an argument naming a sibling worktree must be refused, quoting it; got %v", err)
	}
	e.requireNothingRan(t)
}

// --- when git cannot answer, never guess the tree ------------------------------

// TestMutationTest_AWorktreeUnderANonGitHolderIsRefused: the commands run in a
// directory that is in no repository (a holder with a go.work over several
// checkouts) and the mutant is in a linked worktree below it. The first version
// skipped all checking when the run directory was not in git, and tested the
// worktree mutant with the holder's commands — which reach the main checkout.
func TestMutationTest_AWorktreeUnderANonGitHolderIsRefused(t *testing.T) {
	requireGit(t)
	parent := evalTempDir(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)
	holder := filepath.Join(parent, "holder")
	repo := filepath.Join(holder, "a")
	gwWrite(t, repo, "target.txt", worktreeTargetOriginal)
	gitInit(t, repo)
	wt := filepath.Join(repo, ".claude", "worktrees", "wt")
	gwGit(t, repo, "worktree", "add", "-q", "-b", "wt", wt)
	ran := filepath.Join(t.TempDir(), "ran")
	tool := NewMutationTest(WriteDeps{WorkspaceFn: func(context.Context) string { return holder }},
		func(_ context.Context, slot, _, _ string) (TaskCommand, error) {
			return TaskCommand{Slot: slot, Steps: [][]string{{"/bin/sh", "-c", "echo x >> " + shellQuote(ran)}}, Provenance: "default"}, nil
		})

	_, err := executeMutants(t, tool, filepath.Join(wt, "target.txt"))
	if err == nil {
		t.Fatal("a linked-worktree mutant under commands that run outside every repository must be refused")
	}
	for _, want := range []string{"linked git worktree", wt, "no git repository"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q; got:\n%v", want, err)
		}
	}
	if _, statErr := os.Stat(ran); statErr == nil {
		t.Error("a refusal must come before anything runs")
	}
}

// TestMutationTest_WhenGitCannotAnswer: a checkout git refuses to read (dubious
// ownership — faked here, since the suite may run as root) used to read as "not a
// work-tree, carry on", and a worktree mutant was tested against the main checkout.
// The .git links on disk still say which tree is which: a run that would need
// re-rooting is refused with git's reason; one that stays put runs as it always did.
func TestMutationTest_WhenGitCannotAnswer(t *testing.T) {
	cases := []struct {
		name      string
		mainKills bool
		session   func(e *worktreeEnv) string
		file      func(e *worktreeEnv) string
		refused   bool
	}{
		{"a worktree mutant from the main checkout", false, func(e *worktreeEnv) string { return e.root }, func(e *worktreeEnv) string { return e.file(e.wt) }, true},
		{"a main-checkout mutant from the worktree", true, func(e *worktreeEnv) string { return e.wt }, func(e *worktreeEnv) string { return e.file(e.root) }, true},
		{"a mutant where the commands already run", true, func(e *worktreeEnv) string { return e.root }, func(e *worktreeEnv) string { return e.file(e.root) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newWorktreeEnv(t, "", tc.mainKills, false)
			e.reopenAt(tc.session(e))
			fakeUnusableGit(t)

			out, err := e.run(t, e.mutant(tc.file(e), "42", "43"))
			if tc.refused {
				if err == nil {
					t.Fatalf("with git unable to confirm either tree, a run that needs moving must be refused; got:\n%s", out)
				}
				if !strings.Contains(err.Error(), "dubious ownership") {
					t.Errorf("the refusal must carry git's own reason; got:\n%v", err)
				}
				e.requireNothingRan(t)
				return
			}
			if err != nil {
				t.Fatalf("a mutant where the commands already run must run as it always did: %v", err)
			}
			if !strings.Contains(out, "[1] KILLED") || !strings.Contains(out, "no git safety net") {
				t.Errorf("want a kill plus the no-safety-net warning; got:\n%s", out)
			}
			e.requireAllRanIn(t, e.root)
		})
	}
}

// fakeUnusableGit puts a git first on PATH that refuses every repository the way
// git does for one owned by another user.
func fakeUnusableGit(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	gwWrite(t, bin, "git", "#!/bin/sh\necho \"fatal: detected dubious ownership in repository at '$PWD'\" >&2\nexit 128\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil { //nolint:gosec // G302: the fake must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRerootPlan_ACancelledRequestSaysSo: a cancellation made every git probe fail,
// and the first version then reported the file as "not in any git work-tree".
func TestRerootPlan_ACancelledRequestSaysSo(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := e.file(e.wt)
	_, err := e.tool.rerootPlan(ctx, mutationPlan{
		compile: TaskCommand{Steps: [][]string{{"true"}}},
		test:    TaskCommand{Steps: [][]string{{"true"}}},
	}, []mutationTarget{{path: f, display: f}})
	if err == nil || !strings.Contains(err.Error(), "cancelled") || strings.Contains(err.Error(), "not in any git work-tree") {
		t.Fatalf("a cancelled request must be reported as cancelled; got %v", err)
	}
}

// TestRerootPlan_OneGitProbePerDirectory: five mutants in one file cost one probe
// for the file's directory and one for the commands' directory (shared by compile
// and test); the directory they move to is the file's own here, already probed —
// not one probe per mutant per command, which was 2n+2 before.
func TestRerootPlan_OneGitProbePerDirectory(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "probes")
	gwWrite(t, bin, "git", fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *--show-prefix*) echo probe >> %s;; esac\nexec %s \"$@\"\n", shellQuote(log), shellQuote(realGit)))
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil { //nolint:gosec // G302: the wrapper must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	f := e.file(e.wt)
	if _, err := e.run(t,
		e.mutant(f, "42", "43"), e.mutant(f, "keep", "other"), e.mutant(f, "answer", "answr"),
		e.mutant(f, "label", "labl"), e.mutant(f, "= 4", "= 5"),
	); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "probe"); n != 2 {
		t.Errorf("git was probed %d times, want 2 (the file's directory, which is also the destination, and the commands')", n)
	}
}

// --- nothing is charged, and recovery advice works, wherever the file is -----

// TestMutationTest_RefusalsSpendNoWriteBudget: the re-rooting refusals are instant
// and write nothing, but ran after preflight had charged one write-budget slot per
// mutant — so a few refused calls throttled every write tool on the session.
func TestMutationTest_RefusalsSpendNoWriteBudget(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	limiter := NewRateLimiter(4, time.Minute)
	e.tool.deps.Limiter = limiter

	for range 2 {
		if _, err := e.run(t, e.mutant(e.file(e.root), "42", "43"), e.mutant(e.file(e.wt), "42", "43")); err == nil {
			t.Fatal("mutants in two work-trees must be refused")
		}
	}
	if count, _, _ := limiter.Snapshot(); count != 0 {
		t.Errorf("two refused calls spent %d write slots; a refusal that writes nothing must spend none", count)
	}
	if _, err := e.run(t, e.mutant(e.file(e.wt), "42", "43")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if count, _, _ := limiter.Snapshot(); count != 1 {
		t.Errorf("a run of one mutant spent %d write slots, want 1", count)
	}
}

// TestRestoreFailed_TheRecoveryCommandWorksForAWorktreeFile: the escalation's
// recovery command was `git checkout -- <workspace-relative path>`, which fails from
// every directory for a file in a worktree. It must work as printed, from anywhere.
func TestRestoreFailed_TheRecoveryCommandWorksForAWorktreeFile(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	f := e.file(e.wt)
	gwWrite(t, filepath.Dir(f), filepath.Base(f), "answer = 43\nlabel = keep\n")
	t.Cleanup(func() { _ = os.Remove(f + mutationRestoreSuffix) })

	err := e.tool.restoreFailed(mutationTarget{path: f, display: e.tool.displayPath(f), original: []byte(worktreeTargetOriginal), mode: 0o644}, "forced by the test")
	_, rest, ok := strings.Cut(err.Error(), "recover with: ")
	if !ok {
		t.Fatalf("no recovery command in:\n%v", err)
	}
	command, _, _ := strings.Cut(rest, "   (safe")
	sh := exec.Command("/bin/sh", "-c", command)
	sh.Dir = t.TempDir() // an unrelated directory: the command must not depend on where it is run
	if out, err := sh.CombinedOutput(); err != nil {
		t.Fatalf("the printed recovery command %q failed: %v\n%s", command, err, out)
	}
	requireContent(t, f, worktreeTargetOriginal)
}

// executeMutants runs tool on one 42→43 mutant of path.
func executeMutants(t *testing.T, tool *MutationTest, path string) (string, error) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"mutants": []mutantJSON{{"file_path": path, "old_string": "42", "new_string": "43"}}})
	if err != nil {
		t.Fatal(err)
	}
	return tool.Execute(context.Background(), raw)
}

// gitInit makes root a git repository with everything under it committed. It is
// shared by the mutation_test fixtures.
func gitInit(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		// A developer's global signing or hook directory must not reach a fixture:
		// commit.gpgsign=true fails every fixture commit without a key, and a global
		// core.hooksPath runs hooks the fixture never installed.
		{"config", "commit.gpgsign", "false"},
		{"config", "core.hooksPath", filepath.Join(root, ".git", "hooks")},
		{"add", "-A"},
		{"commit", "-q", "-m", "fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
