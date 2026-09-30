package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mutationtest_root_test.go covers PLAN-442's second defect: mutation_test ran
// the workspace's stored compile and test commands from the workspace's own
// directory, so a mutant in a git WORKTREE was tested against the main checkout —
// every mutant read SURVIVED with `compile ok` and `tests ok`, which is
// indistinguishable from "your assertions are vacuous".
//
// The fixture makes the two trees DISAGREE on purpose. The test script of one
// tree kills a mutant that changes `42` to `43`; the other tree's passes whatever
// it is given. A run that used the wrong tree therefore cannot produce the right
// verdict by accident, and every script writes the directory it ran in to a log
// outside both trees, so "where did it run" is read from the process, not
// inferred from the verdict.

// worktreeEnv is a workspace (the main checkout) with a linked git worktree of it
// living inside it — the layout an agent isolation setup produces.
type worktreeEnv struct {
	root string // the workspace: the main checkout
	wt   string // the linked worktree
	sub  string // directory, relative to each tree, holding target.txt and the scripts
	log  string // every script appends "<cwd> <script> <GOWORK>" here
	tool *MutationTest
	// workdir is what the resolver reports as the commands' working_dir; "" means
	// the workspace root.
	workdir string
	// testArgv, when set, replaces the test command's argv (default: sh test.sh).
	testArgv []string
}

// newWorktreeEnv builds the two trees. mainKills says which tree's test script
// kills the `43` mutant; the other passes unconditionally. wtCompileRed makes the
// worktree's compile gate fail while the main checkout's passes.
func newWorktreeEnv(t *testing.T, sub string, mainKills, wtCompileRed bool) *worktreeEnv {
	t.Helper()
	requireGit(t)
	// The scripts record the GOWORK they were started with, so the daemon this models
	// must not have one of its own unless a test gives it one.
	unsetEnvForTest(t, "GOWORK")
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	logDir := t.TempDir()
	e := &worktreeEnv{root: root, sub: sub, log: filepath.Join(logDir, "ran.log")}

	gwWrite(t, filepath.Join(root, sub), "target.txt", "answer = 42\nlabel = keep\n")
	e.writeScripts(t, filepath.Join(root, sub), mainKills, false)
	gitInit(t, root)

	e.wt = filepath.Join(root, ".claude", "worktrees", "wt")
	gwGit(t, root, "worktree", "add", "-q", "-b", "wt", e.wt)
	wtDir := filepath.Join(e.wt, sub)
	e.writeScripts(t, wtDir, !mainKills, wtCompileRed)
	gwGit(t, e.wt, "add", "-A")
	gwGit(t, e.wt, "commit", "-q", "-m", "worktree scripts")

	e.reopenAt(root)
	return e
}

// reopenAt rebuilds the tool with workspace as the connection's workspace root —
// the main checkout's path by default, or another spelling of it.
func (e *worktreeEnv) reopenAt(workspace string) {
	deps := WriteDeps{WorkspaceFn: func(context.Context) string { return workspace }}
	e.tool = NewMutationTest(deps, func(_ context.Context, req TaskRequest) (TaskCommand, error) {
		slot := req.Slot
		argv := []string{"/bin/sh", "compile.sh"}
		if slot == "test" {
			argv = []string{"/bin/sh", "test.sh"}
			if e.testArgv != nil {
				argv = e.testArgv
			}
		}
		return TaskCommand{Slot: slot, Steps: [][]string{argv}, Provenance: "default", WorkingDir: e.workdir}, nil
	})
}

// writeScripts installs compile.sh and test.sh in dir. Both log where they ran.
// The test script fails only when the `43` mutant is on disk AND kills is set.
func (e *worktreeEnv) writeScripts(t *testing.T, dir string, kills, compileRed bool) {
	t.Helper()
	logLine := fmt.Sprintf("echo \"$(/bin/pwd) $0 ${GOWORK-UNSET}\" >> %q\n", e.log)
	compile := logLine + "exit 0\n"
	if compileRed {
		compile = logLine + "echo 'compile gate is red here'\nexit 1\n"
	}
	test := logLine
	if kills {
		test += "grep -q '43' target.txt && { echo '--- FAIL: TestAnswer (0.00s)'; exit 1; }\n"
	}
	test += "exit 0\n"
	gwWrite(t, dir, "compile.sh", compile)
	gwWrite(t, dir, "test.sh", test)
}

func (e *worktreeEnv) file(tree string) string { return filepath.Join(tree, e.sub, "target.txt") }

type mutantJSON = map[string]any

func (e *worktreeEnv) mutant(path, old, replacement string) mutantJSON {
	return mutantJSON{"file_path": path, "old_string": old, "new_string": replacement}
}

func (e *worktreeEnv) run(t *testing.T, mutants ...mutantJSON) (string, error) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"mutants": mutants})
	if err != nil {
		t.Fatal(err)
	}
	return e.tool.Execute(context.Background(), raw)
}

// ranIn returns the directories the scripts ran in, one entry per run, in order.
func (e *worktreeEnv) ranIn(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(e.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		dir, _, _ := strings.Cut(line, " ")
		dirs = append(dirs, dir)
	}
	return dirs
}

// ranGOWORK returns the GOWORK each script run saw — "UNSET" when the variable was
// absent, which is not the same thing as empty. It fails when nothing ran, so an
// empty answer cannot pass a loop over it.
func (e *worktreeEnv) ranGOWORK(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(e.log)
	if err != nil {
		t.Fatalf("no script ran, so nothing can be said about its environment: %v", err)
	}
	var seen []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if fields := strings.Fields(line); len(fields) == 3 {
			seen = append(seen, fields[2])
		}
	}
	if len(seen) == 0 {
		t.Fatalf("the log has no complete line to read GOWORK from:\n%s", b)
	}
	return seen
}

// requireAllRanIn fails unless at least one script ran and every one ran in want.
func (e *worktreeEnv) requireAllRanIn(t *testing.T, want string) {
	t.Helper()
	dirs := e.ranIn(t)
	if len(dirs) == 0 {
		t.Fatal("no script ran at all, so nothing can be said about where the commands ran")
	}
	for _, d := range dirs {
		if d != want {
			t.Errorf("a command ran in %s, want every one in %s", d, want)
		}
	}
}

func (e *worktreeEnv) requireNothingRan(t *testing.T) {
	t.Helper()
	if dirs := e.ranIn(t); len(dirs) != 0 {
		t.Errorf("a refusal must come before anything runs, but commands ran in %v", dirs)
	}
}

func requireContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want the original %q back", path, got, want)
	}
}

const worktreeTargetOriginal = "answer = 42\nlabel = keep\n"

// --- the acceptance: a worktree's mutants run the worktree's tests -----------

// TestMutationTest_AWorktreeMutantRunsTheWorktreesTests is defect 2 of PLAN-442,
// both directions in ONE fixture. Two mutants in the worktree: `42`→`43` is
// covered by the worktree's test and must read KILLED; `keep`→`other` is covered
// by nothing and must still read SURVIVED. The main checkout's test passes
// unconditionally, so the run that used to happen (main's tests, against a file
// they never see) reports both SURVIVED — which is how the tool gave a verdict
// nothing stood behind.
func TestMutationTest_AWorktreeMutantRunsTheWorktreesTests(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	f := e.file(e.wt)

	out, err := e.run(t,
		e.mutant(f, "42", "43"),
		e.mutant(f, "keep", "other"),
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") || !strings.Contains(out, "killed by: TestAnswer") {
		t.Errorf("the mutant the worktree's own test catches must be KILLED by that test; got:\n%s", out)
	}
	if !strings.Contains(out, "[2] SURVIVED") {
		t.Errorf("the mutant no test covers must still read SURVIVED; got:\n%s", out)
	}
	if !strings.Contains(out, "summary: 1 killed · 1 survived · 0 invalid") {
		t.Errorf("summary must count one kill and one survivor; got:\n%s", out)
	}
	// Where it ran is read from the process, not the verdict: main's script never ran.
	e.requireAllRanIn(t, e.wt)
	// And the report says so, so a reader is never left guessing which tree spoke.
	if !strings.Contains(out, "ran in: "+e.wt) || !strings.Contains(out, "re-rooted") {
		t.Errorf("the report must name the work-tree the commands were re-rooted to (%s); got:\n%s", e.wt, out)
	}
	requireContent(t, f, worktreeTargetOriginal)
	requireContent(t, e.file(e.root), worktreeTargetOriginal)
}

// TestMutationTest_AWorkspaceMutantStillRunsInTheWorkspace is the control that
// the re-rooting is not applied to a file that was already in the right tree. Here
// the MAIN checkout's test kills and the worktree's passes, so a run that was
// moved to the worktree could not kill.
func TestMutationTest_AWorkspaceMutantStillRunsInTheWorkspace(t *testing.T) {
	e := newWorktreeEnv(t, "", true, false)

	out, err := e.run(t, e.mutant(e.file(e.root), "42", "43"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") {
		t.Errorf("a mutant in the workspace must run the workspace's tests and be KILLED by them; got:\n%s", out)
	}
	e.requireAllRanIn(t, e.root)
	if strings.Contains(out, "re-rooted") || strings.Contains(out, "ran in:") {
		t.Errorf("nothing was re-rooted, so the report must not say so; got:\n%s", out)
	}
	requireContent(t, e.file(e.root), worktreeTargetOriginal)
}

// TestMutationTest_RerootKeepsTheRelativeWorkingDir: the resolver's working_dir is
// a subdirectory (the holder-repository shape), and the worktree gets the SAME
// relative directory under its own root, not the main checkout's absolute path.
func TestMutationTest_RerootKeepsTheRelativeWorkingDir(t *testing.T) {
	e := newWorktreeEnv(t, "module", false, false)
	e.workdir = filepath.Join(e.root, "module")

	out, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") {
		t.Errorf("the worktree's module test must kill the mutant; got:\n%s", out)
	}
	e.requireAllRanIn(t, filepath.Join(e.wt, "module"))
}

// TestMutationTest_ADirectoryRecasedOnTheWorktreesBranchStillReRoots: on a
// case-insensitive volume the worktree's branch may spell the working directory in
// another case (Module/ renamed to module/). It is the same directory, and git
// names it in the worktree's case; refusing it as "a symlink on its branch" was
// a false refusal.
func TestMutationTest_ADirectoryRecasedOnTheWorktreesBranchStillReRoots(t *testing.T) {
	e := newWorktreeEnv(t, "Module", false, false)
	e.workdir = filepath.Join(e.root, "Module")
	gwGit(t, e.wt, "mv", "Module", "recase-tmp")
	gwGit(t, e.wt, "mv", "recase-tmp", "module")
	gwGit(t, e.wt, "commit", "-q", "-m", "recase")
	if _, err := os.Stat(filepath.Join(e.wt, "Module")); err != nil {
		t.Skip("this volume is case-sensitive: the worktree has no Module/ at all")
	}

	out, err := e.run(t, e.mutant(filepath.Join(e.wt, "module", "target.txt"), "42", "43"))
	if err != nil {
		t.Fatalf("the recased directory is the same directory and must be re-rooted into: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") || !strings.Contains(out, "re-rooted") {
		t.Errorf("the worktree's test must kill the mutant after a re-root; got:\n%s", out)
	}
	for _, d := range e.ranIn(t) {
		if !strings.EqualFold(d, filepath.Join(e.wt, "module")) {
			t.Errorf("a command ran in %s, want the worktree's module directory", d)
		}
	}
}

// --- refusals: never a silent wrong-tree run ---------------------------------

func TestMutationTest_MutantsInTwoWorkTreesAreRefused(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)

	_, err := e.run(t,
		e.mutant(e.file(e.root), "42", "43"),
		e.mutant(e.file(e.wt), "42", "43"),
	)
	if err == nil {
		t.Fatal("mutants in two work-trees must be refused: the commands run in one directory, so one file is tested against the wrong tree")
	}
	for _, want := range []string{"more than one git work-tree", e.root, e.wt} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q; got:\n%v", want, err)
		}
	}
	e.requireNothingRan(t)
	requireContent(t, e.file(e.root), worktreeTargetOriginal)
	requireContent(t, e.file(e.wt), worktreeTargetOriginal)
}

func TestMutationTest_AWorkingDirTheWorktreeLacksIsRefused(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	// A directory that exists only in the main checkout (untracked, so the
	// worktree never received it): the commands' working_dir there has no
	// counterpart in the worktree.
	scratch := filepath.Join(e.root, "scratch")
	e.writeScripts(t, scratch, false, false)
	e.workdir = scratch

	_, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err == nil {
		t.Fatal("a working_dir the worktree does not have must be refused, not run somewhere else")
	}
	for _, want := range []string{"has no such directory", `"scratch"`, e.wt} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q; got:\n%v", want, err)
		}
	}
	e.requireNothingRan(t)
	requireContent(t, e.file(e.wt), worktreeTargetOriginal)
}

// TestMutationTest_AnArgumentNamingTheOtherTreeIsRefused: moving the working
// directory does nothing for a command that spells an absolute path into the tree
// it left — the script below is the MAIN checkout's, by absolute path, so a run
// re-rooted around it would execute the wrong tree's tests while reporting the
// right directory.
func TestMutationTest_AnArgumentNamingTheOtherTreeIsRefused(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	script := filepath.Join(e.root, "test.sh")
	e.testArgv = []string{"/bin/sh", script}

	_, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err == nil {
		t.Fatal("a command argument naming a path in the tree being left must be refused")
	}
	if !strings.Contains(err.Error(), script) || !strings.Contains(err.Error(), "argument") {
		t.Errorf("the refusal must quote the offending argument (%s); got:\n%v", script, err)
	}
	e.requireNothingRan(t)
	requireContent(t, e.file(e.wt), worktreeTargetOriginal)
}

// TestMutationTest_AFileInAnotherRepositorysMainTreeRunsInPlace: a repository
// nested in the workspace (a submodule, a cloned dependency) in its MAIN work-tree
// has no other copy of its files, so commands that reach it test it — the behaviour
// before re-rooting existed, and the one a superproject's `go test ./sub/...` or
// `make -C lib test` depends on. Refusing it (as the first version did) broke runs
// that had valid verdicts, with no remedy for a sibling repository.
func TestMutationTest_AFileInAnotherRepositorysMainTreeRunsInPlace(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	other := filepath.Join(e.root, "other")
	gwWrite(t, other, "target.txt", worktreeTargetOriginal)
	gitInit(t, other)
	// The workspace's test reaches into the nested repository, as a superproject's does.
	e.testArgv = []string{"/bin/sh", "-c", "echo \"$(/bin/pwd) $0 ${GOWORK-UNSET}\" >> " + shellQuote(e.log) +
		"; grep -q 43 other/target.txt && { echo '--- FAIL: TestNested (0.00s)'; exit 1; }; exit 0"}

	out, err := e.run(t, e.mutant(filepath.Join(other, "target.txt"), "42", "43"))
	if err != nil {
		t.Fatalf("a file in another repository's main work-tree must run where the commands already run: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") {
		t.Errorf("the workspace's test reaches the nested file and must kill the mutant; got:\n%s", out)
	}
	if strings.Contains(out, "re-rooted") {
		t.Errorf("nothing may be re-rooted into another repository; got:\n%s", out)
	}
	e.requireAllRanIn(t, e.root)
	requireContent(t, filepath.Join(other, "target.txt"), worktreeTargetOriginal)
}

// TestMutationTest_AnotherRepositorysMainTreeSpelledInAnotherCaseRunsInPlace: the
// same run with the nested repository spelled in a case the disk does not use, as
// a client on macOS's case-insensitive volume may. It must not be mistaken for a
// linked worktree and refused (see TestProbeGitDir_ACaseVariantSpellingIsTheSameTree).
func TestMutationTest_AnotherRepositorysMainTreeSpelledInAnotherCaseRunsInPlace(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	other := filepath.Join(e.root, "Other")
	gwWrite(t, filepath.Join(other, "pkg"), "target.txt", worktreeTargetOriginal)
	gitInit(t, other)
	flipped := filepath.Join(e.root, "oTHER", "pkg", "target.txt")
	if _, err := os.Stat(flipped); err != nil {
		t.Skip("this volume is case-sensitive: no other spelling of the file exists")
	}
	e.testArgv = []string{"/bin/sh", "-c", "echo \"$(/bin/pwd) $0 ${GOWORK-UNSET}\" >> " + shellQuote(e.log) +
		"; grep -q 43 Other/pkg/target.txt && { echo '--- FAIL: TestNested (0.00s)'; exit 1; }; exit 0"}

	out, err := e.run(t, e.mutant(flipped, "42", "43"))
	if err != nil {
		t.Fatalf("a file in another repository's main work-tree must run in place whatever the case it is spelled in: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") || strings.Contains(out, "re-rooted") {
		t.Errorf("the workspace's test must kill the mutant where it already runs; got:\n%s", out)
	}
	e.requireAllRanIn(t, e.root)
	requireContent(t, filepath.Join(other, "pkg", "target.txt"), worktreeTargetOriginal)
}

// TestMutationTest_AFileInALinkedWorktreeOfAnotherRepositoryIsRefused: a LINKED
// worktree of a repository the commands do not run in always has a twin — that
// repository's main work-tree — which the commands would reach instead. It cannot
// be re-rooted into either (the workspace's trusted commands were never configured
// for that repository), so it is refused.
func TestMutationTest_AFileInALinkedWorktreeOfAnotherRepositoryIsRefused(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	other := filepath.Join(e.root, "other")
	gwWrite(t, other, "target.txt", worktreeTargetOriginal)
	gitInit(t, other)
	otherWT := filepath.Join(other, ".claude", "worktrees", "owt")
	gwGit(t, other, "worktree", "add", "-q", "-b", "owt", otherWT)

	_, err := e.run(t, e.mutant(filepath.Join(otherWT, "target.txt"), "42", "43"))
	if err == nil {
		t.Fatal("a file in a linked worktree of another repository must be refused")
	}
	for _, want := range []string{"linked git worktree", otherWT, "different repository", e.root} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q; got:\n%v", want, err)
		}
	}
	e.requireNothingRan(t)
	requireContent(t, filepath.Join(otherWT, "target.txt"), worktreeTargetOriginal)
}

// TestMutationTest_AFileOutsideEveryWorkTreeRunsInPlace: a file in no repository
// has no twin in any tree, so it runs where the commands run — with the existing
// "no git safety net" warning, which the first version of re-rooting made
// unreachable by refusing the run instead.
func TestMutationTest_AFileOutsideEveryWorkTreeRunsInPlace(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	parent := evalTempDir(t)
	plain := filepath.Join(parent, "plain")
	gwWrite(t, plain, "target.txt", worktreeTargetOriginal)
	// `make test` puts t.TempDir() under the repository's own .testcache, so git
	// would otherwise find plumb's repository above "plain".
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)

	out, err := e.run(t, e.mutant(filepath.Join(plain, "target.txt"), "42", "43"))
	if err != nil {
		t.Fatalf("a file in no git work-tree must run where the commands run: %v", err)
	}
	if !strings.Contains(out, "no git safety net") {
		t.Errorf("the no-safety-net warning must be reported; got:\n%s", out)
	}
	e.requireAllRanIn(t, e.root)
	requireContent(t, filepath.Join(plain, "target.txt"), worktreeTargetOriginal)
}

// TestMutationTest_AWorktreeMutantBesideAFileInNoRepositoryIsRefused: one mutant
// needs the worktree, the other stays where the commands are — two places at once.
func TestMutationTest_AWorktreeMutantBesideAFileInNoRepositoryIsRefused(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	parent := evalTempDir(t)
	plain := filepath.Join(parent, "plain")
	gwWrite(t, plain, "target.txt", worktreeTargetOriginal)
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)

	_, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"), e.mutant(filepath.Join(plain, "target.txt"), "42", "43"))
	if err == nil || !strings.Contains(err.Error(), "more than one git work-tree") {
		t.Fatalf("mutants that need the commands in two places must be refused; got %v", err)
	}
	e.requireNothingRan(t)
}

// TestBaseline_NamesTheRerootedDirectory: a baseline refusal in the worktree must
// say the command ran IN the worktree and why — not fall back to "the workspace
// root", which is the directory it did NOT run in.
func TestBaseline_NamesTheRerootedDirectory(t *testing.T) {
	e := newWorktreeEnv(t, "", false, true) // the worktree's compile gate is red

	_, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err == nil {
		t.Fatal("a red compile gate in the worktree must refuse the run at the baseline")
	}
	msg := err.Error()
	if !strings.Contains(msg, "in "+e.wt+" ") {
		t.Errorf("the refusal must name the directory it ran IN (%s); got:\n%s", e.wt, msg)
	}
	if !strings.Contains(msg, "work-tree that contains the mutated file") {
		t.Errorf("the refusal must say why that directory is the cwd; got:\n%s", msg)
	}
	if strings.Contains(msg, "WORKSPACE ROOT") || strings.Contains(msg, "WORKING DIR set by") {
		t.Errorf("the refusal must not attribute the directory to the workspace root or to working_dir; got:\n%s", msg)
	}
	e.requireAllRanIn(t, e.wt)
	requireContent(t, e.file(e.wt), worktreeTargetOriginal)
}

// --- the pieces ---------------------------------------------------------------

// TestProbeGitDir pins the identity the same-repository rule rests on: every
// work-tree of one repository shares a common git directory, no two repositories
// do, a linked worktree is told from a main work-tree, and the relative directory
// is git's own.
func TestProbeGitDir(t *testing.T) {
	e := newWorktreeEnv(t, "module", false, false)
	other := filepath.Join(e.root, "other")
	gwWrite(t, other, "x.txt", "x\n")
	gitInit(t, other)
	ctx := context.Background()
	tree := func(dir string) gitTree {
		t.Helper()
		p := probeGitDir(ctx, dir)
		if p.place != placeTree {
			t.Fatalf("probeGitDir(%s) = %+v, want a work-tree", dir, p)
		}
		return p.tree
	}
	mainTree, wtTree, otherTree := tree(e.root), tree(e.wt), tree(other)
	if mainTree.top != e.root || wtTree.top != e.wt || otherTree.top != other {
		t.Errorf("tops = %s, %s, %s; want %s, %s, %s", mainTree.top, wtTree.top, otherTree.top, e.root, e.wt, other)
	}
	if wtTree.common != mainTree.common {
		t.Errorf("a worktree must share its repository's common dir: %s vs %s", wtTree.common, mainTree.common)
	}
	if otherTree.common == mainTree.common {
		t.Errorf("two repositories must not share a common dir (%s)", otherTree.common)
	}
	if mainTree.linked || !wtTree.linked || otherTree.linked {
		t.Errorf("linked = %v, %v, %v; want false, true, false", mainTree.linked, wtTree.linked, otherTree.linked)
	}
	if sub := tree(filepath.Join(e.wt, "module")); sub.top != e.wt || sub.prefix != "module/" {
		t.Errorf("a subdirectory: got %+v, want top %s prefix module/", sub, e.wt)
	}
	if mainTree.prefix != "" {
		t.Errorf("the root's prefix = %q, want empty", mainTree.prefix)
	}

	// A relative common dir is relative to the PHYSICAL directory: asked from under
	// an in-repo symlink, "../.git"-style answers must still name this repository.
	link := filepath.Join(e.root, "modlink")
	if err := os.Symlink(filepath.Join(e.root, "module"), link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if viaLink := tree(link); viaLink.common != mainTree.common || viaLink.prefix != "module/" {
		t.Errorf("through an in-repo symlink: got %+v, want common %s prefix module/", viaLink, mainTree.common)
	}

	parent := evalTempDir(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)
	// Probe BELOW the ceiling: git still resolves the ceiling directory itself up
	// to an enclosing repository, and `make test` puts t.TempDir() inside this one.
	plain := filepath.Join(parent, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if p := probeGitDir(ctx, plain); p.place != placeNoRepo {
		t.Errorf("a directory in no repository: got %+v, want placeNoRepo", p)
	}
	if p := probeGitDir(ctx, filepath.Join(parent, "no", "such")); p.place != placeUnknown || p.reason == "" {
		t.Errorf("a path git cannot enter must be unknown with git's reason, never 'no repository': got %+v", p)
	}
}

// TestProbeGitDir_ACaseVariantSpellingIsTheSameTree: on a case-insensitive volume
// (macOS's default) a client may spell a directory in a case the disk does not
// use. Below a main work-tree's root git prints --git-dir absolute, in the disk's
// case, but --git-common-dir relative ("../.git"); joined to the client's spelling
// the two named one directory in two cases, and the main work-tree read as a
// LINKED worktree — so a file in another repository's main work-tree was refused
// as stranded instead of running in place.
func TestProbeGitDir_ACaseVariantSpellingIsTheSameTree(t *testing.T) {
	requireGit(t)
	parent := evalTempDir(t)
	repo := filepath.Join(parent, "Repo")
	gwWrite(t, filepath.Join(repo, "Pkg"), "x.txt", "x\n")
	gitInit(t, repo)
	flipped := filepath.Join(parent, "rEPO", "pKG")
	if a, err := os.Stat(filepath.Join(repo, "Pkg")); err != nil {
		t.Fatal(err)
	} else if b, err := os.Stat(flipped); err != nil || !os.SameFile(a, b) {
		t.Skip("this volume is case-sensitive: no other spelling of the directory exists")
	}

	ctx := context.Background()
	asSpelled, flippedP := probeGitDir(ctx, filepath.Join(repo, "Pkg")), probeGitDir(ctx, flipped)
	if asSpelled.place != placeTree || flippedP.place != placeTree {
		t.Fatalf("both spellings must be in a work-tree: %+v, %+v", asSpelled, flippedP)
	}
	if flippedP.tree.linked {
		t.Errorf("a main work-tree spelled in another case must not read as a linked worktree: %+v", flippedP.tree)
	}
	if flippedP.tree != asSpelled.tree {
		t.Errorf("two spellings of one directory must get git's one answer:\n%+v\n%+v", flippedP.tree, asSpelled.tree)
	}
}

// TestDiskProbe covers the fallback read of .git when git cannot answer: a main
// work-tree, a linked worktree (its gitdir holds `commondir`), a submodule-style
// link (no `commondir`) and a link that cannot be read.
func TestDiskProbe(t *testing.T) {
	e := newWorktreeEnv(t, "module", false, false)
	mainGit := probeGitDir(context.Background(), e.root).tree.common

	if p := diskProbe(filepath.Join(e.root, "module"), "why"); p.place != placeTree || p.tree.top != e.root || p.tree.common != mainGit || p.tree.linked || !p.approx {
		t.Errorf("main work-tree: got %+v", p)
	}
	if p := diskProbe(filepath.Join(e.wt, "module"), "why"); p.place != placeTree || p.tree.top != e.wt || p.tree.common != mainGit || !p.tree.linked {
		t.Errorf("linked worktree: got %+v, want linked with common %s", p, mainGit)
	}
	sub := evalTempDir(t)
	modules := filepath.Join(sub, "super", ".git", "modules", "lib")
	writeTree(t, modules, map[string]string{"HEAD": "ref: refs/heads/main\n"})
	writeTree(t, sub, map[string]string{"super/lib/.git": "gitdir: ../.git/modules/lib\n", "super/lib/x.txt": "x"})
	if p := diskProbe(filepath.Join(sub, "super", "lib"), "why"); p.place != placeTree || p.tree.linked || p.tree.common != modules {
		t.Errorf("submodule link: got %+v, want not linked, common %s", p, modules)
	}
	writeTree(t, sub, map[string]string{"broken/.git": "not a gitdir line\n"})
	if p := diskProbe(filepath.Join(sub, "broken"), "why"); p.place != placeUnknown {
		t.Errorf("an unreadable .git link must be unknown (the answer that refuses): got %+v", p)
	}
}

func TestArgNamingTree(t *testing.T) {
	const (
		main = "/r/plumb"                            // the main checkout
		wt   = "/r/plumb/.claude/worktrees/wt"       // a worktree nested inside it
		peer = "/r/plumb/.claude/worktrees/wt-old/t" // a sibling worktree whose name starts with wt's
		sib  = "/r/plumb-tools/bin/gotestsum"        // a sibling directory sharing main's name prefix
		app  = "/app"                                // a short root
	)
	cases := []struct {
		name       string
		from, dest string
		argv       []string
		want       string
	}{
		{"relative arguments move with the directory", main, wt, []string{"go", "test", "./internal/x/..."}, ""},
		{"a path in the tree being left", main, wt, []string{"sh", main + "/scripts/test.sh"}, main + "/scripts/test.sh"},
		{"a path in the tree being entered (nested in the one left)", main, wt, []string{"sh", wt + "/scripts/test.sh"}, ""},
		// Substring matching skipped this — it CONTAINS wt's path — and ran the
		// other worktree's script: SURVIVED from the wrong tree.
		{"a sibling worktree whose name extends the destination's", main, wt, []string{"/bin/sh", peer + "/test.sh"}, peer + "/test.sh"},
		// Leaving the nested worktree for the main checkout: a path in the worktree
		// is under BOTH roots, and the more specific one — the tree being left — wins.
		{"a path in the nested worktree being left", wt, main, []string{"sh", wt + "/test.sh"}, wt + "/test.sh"},
		{"a path elsewhere in the main checkout being entered", wt, main, []string{"sh", main + "/test.sh"}, ""},
		{"a sibling directory sharing a name prefix is not inside", main, wt, []string{sib}, ""},
		{"a relative argument containing a short root is not a path into it", app, app + "/wt", []string{"go", "test", "./internal/app/..."}, ""},
		{"a flag value", main, wt, []string{"go", "test", "-coverprofile=" + main + "/c.out"}, "-coverprofile=" + main + "/c.out"},
		{"a list value", main, wt, []string{"env", "PATHS=/usr/bin:" + main + "/bin"}, "PATHS=/usr/bin:" + main + "/bin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := argNamingTree(TaskCommand{Steps: [][]string{tc.argv}}, tc.from, tc.dest)
			if got != tc.want || ok != (tc.want != "") {
				t.Errorf("argNamingTree = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.want != "")
			}
		})
	}
}

// TestArgNamingTree_ResolvesSymlinks: an argument spelling the tree being left
// through a link is still a path into it.
func TestArgNamingTree_ResolvesSymlinks(t *testing.T) {
	root := evalTempDir(t)
	main, wt := filepath.Join(root, "main"), filepath.Join(root, "main", "wt")
	writeTree(t, root, map[string]string{"main/wt/x": "x", "main/test.sh": "x"})
	link := filepath.Join(root, "alias")
	if err := os.Symlink(main, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	arg := filepath.Join(link, "test.sh")
	if got, ok := argNamingTree(TaskCommand{Steps: [][]string{{"sh", arg}}}, main, wt); !ok || got != arg {
		t.Errorf("a link-spelled path into the tree being left must be found; got (%q, %v)", got, ok)
	}
}

// TestPlanDirNote: a re-rooted command is described as re-rooted; a command in the
// same plan that was NOT moved, and any command in a plan with nothing re-rooted, keep
// runDirNote's description of the workspace root — calling either a re-rooted
// directory would be the false attribution this note exists to prevent.
func TestPlanDirNote(t *testing.T) {
	root := t.TempDir()
	tool := NewMutationTest(WriteDeps{WorkspaceFn: func(context.Context) string { return root }}, nil)
	dest := filepath.Join(root, ".claude", "worktrees", "wt")
	plan := mutationPlan{reroots: []mutationReroot{{from: root, dir: dest}}}
	moved := TaskCommand{Steps: [][]string{{"/bin/sh", "test.sh"}}, WorkingDir: dest}
	unmoved := TaskCommand{Steps: [][]string{{"/bin/sh", "compile.sh"}}}
	ctx := context.Background()

	if got := tool.planDirNote(ctx, plan, moved, 0); !strings.Contains(got, "re-rooted") || !strings.Contains(got, "in "+dest+" ") {
		t.Errorf("a moved command must be described as re-rooted, in %s; got %q", dest, got)
	}
	if got := tool.planDirNote(ctx, plan, unmoved, 0); strings.Contains(got, "re-rooted") || !strings.Contains(got, "WORKSPACE ROOT") {
		t.Errorf("a command that was not moved must keep the workspace-root description; got %q", got)
	}
	if got := tool.planDirNote(ctx, mutationPlan{}, moved, 0); strings.Contains(got, "re-rooted") {
		t.Errorf("a plan with nothing re-rooted must not claim it was; got %q", got)
	}
}

// TestMutationTest_NoWorkTreeAtAllRunsAsBefore: when the commands' own directory is
// in no git work-tree there is nothing to compare a file's tree against, and the run
// is unchanged — including the existing "no git safety net" warning.
func TestMutationTest_NoWorkTreeAtAllRunsAsBefore(t *testing.T) {
	requireGit(t)
	parent := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = resolved
	}
	plain := filepath.Join(parent, "plain")
	log := filepath.Join(t.TempDir(), "ran.log")
	gwWrite(t, plain, "target.txt", worktreeTargetOriginal)
	gwWrite(t, plain, "compile.sh", fmt.Sprintf("echo \"$(/bin/pwd) compile\" >> %q\nexit 0\n", log))
	gwWrite(t, plain, "test.sh", fmt.Sprintf("echo \"$(/bin/pwd) test\" >> %q\ngrep -q '43' target.txt && { echo '--- FAIL: TestAnswer (0.00s)'; exit 1; }\nexit 0\n", log))
	// See TestMutationTest_AFileOutsideEveryWorkTreeIsRefused: without a ceiling git
	// finds plumb's own repository above the temp directory under `make test`.
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)

	tool := NewMutationTest(WriteDeps{WorkspaceFn: func(context.Context) string { return plain }},
		func(_ context.Context, req TaskRequest) (TaskCommand, error) {
			slot := req.Slot
			script := "compile.sh"
			if slot == "test" {
				script = "test.sh"
			}
			return TaskCommand{Slot: slot, Steps: [][]string{{"/bin/sh", script}}, Provenance: "default", WorkingDir: plain}, nil
		})
	raw, err := json.Marshal(map[string]any{"mutants": []mutantJSON{{"file_path": filepath.Join(plain, "target.txt"), "old_string": "42", "new_string": "43"}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("a run with no git work-tree anywhere must proceed as it always did: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") || !strings.Contains(out, "no git safety net") {
		t.Errorf("expected a kill and the existing no-safety-net warning; got:\n%s", out)
	}
	if strings.Contains(out, "re-rooted") {
		t.Errorf("nothing can be re-rooted without a work-tree; got:\n%s", out)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if dir, _, _ := strings.Cut(line, " "); dir != plain {
			t.Errorf("a command ran in %s, want %s", dir, plain)
		}
	}
}

// --- the re-rooted commands and the enclosing go.work -------------------------

// underGoWork puts the fixture in the layout that motivated PLAN-442: both trees
// are Go modules, and a go.work ABOVE the workspace lists the main checkout's
// module but not the worktree's. A `go build ./...` from the worktree is refused
// there ("directory prefix . does not contain modules listed in go.work"), so a
// re-rooted command that inherits the daemon's environment fails its baseline for a
// reason that has nothing to do with the code — and under a message calling the
// build broken. The scripts record the GOWORK they saw, which is the observation.
func (e *worktreeEnv) underGoWork(t *testing.T) {
	t.Helper()
	for _, tree := range []string{e.root, e.wt} {
		gwWrite(t, tree, "go.mod", "module example.com/reroot\n\ngo 1.21\n")
		gwGit(t, tree, "add", "go.mod")
		gwGit(t, tree, "commit", "-q", "-m", "go.mod")
	}
	// t.TempDir() returns <base>/001 under a base directory unique to this test, so
	// the go.work written beside it belongs to this fixture alone.
	gwWrite(t, filepath.Dir(e.root), "go.work", "go 1.21\n\nuse ./"+filepath.Base(e.root)+"\n")
}

// TestMutationTest_AWorkspaceReachedThroughASymlinkStillReRoots: the workspace path
// the connection reports is a symlink to the main checkout (macOS /tmp, a linked
// projects directory). git prints the worktree's common directory absolute but the
// main checkout's relative to the directory it was asked from, so the two only
// compare equal once both are resolved — otherwise the same repository is refused as
// "a different repository".
func TestMutationTest_AWorkspaceReachedThroughASymlinkStillReRoots(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	alias := filepath.Join(filepath.Dir(e.root), "alias")
	if err := os.Symlink(e.root, alias); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	e.reopenAt(alias)

	out, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err != nil {
		t.Fatalf("a workspace reached through a symlink must still re-root into its own worktree: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") {
		t.Errorf("the worktree's test must kill the mutant; got:\n%s", out)
	}
	e.requireAllRanIn(t, e.wt)
}

// TestMutationTest_AReRootedRunDoesNotInheritAGoWorkThatExcludesTheWorktree: the
// commands moved into the worktree run with GOWORK=off, exactly as the git tool's
// hooks do (git_gowork.go), and the report says so.
func TestMutationTest_AReRootedRunDoesNotInheritAGoWorkThatExcludesTheWorktree(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	e.underGoWork(t)

	out, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "[1] KILLED") {
		t.Errorf("the worktree's own test must kill the mutant; got:\n%s", out)
	}
	e.requireAllRanIn(t, e.wt)
	for _, got := range e.ranGOWORK(t) {
		if got != "off" {
			t.Errorf("a re-rooted command saw GOWORK=%q, want \"off\": the enclosing go.work does not list the worktree", got)
		}
	}
	if !strings.Contains(out, "GOWORK=off") {
		t.Errorf("the report must say the commands ran with GOWORK=off; got:\n%s", out)
	}
}

// TestMutationTest_AWorkspaceRunKeepsItsGoWork is the control: a mutant in the
// workspace's own tree, whose module the go.work lists, is not re-rooted and does
// not get GOWORK=off — that would detach it from the workspace it depends on.
func TestMutationTest_AWorkspaceRunKeepsItsGoWork(t *testing.T) {
	e := newWorktreeEnv(t, "", true, false)
	e.underGoWork(t)

	out, err := e.run(t, e.mutant(e.file(e.root), "42", "43"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	e.requireAllRanIn(t, e.root)
	for _, got := range e.ranGOWORK(t) {
		if got != "UNSET" {
			t.Errorf("a command in the listed workspace saw GOWORK=%q, want it untouched (UNSET)", got)
		}
	}
	if strings.Contains(out, "GOWORK=off") {
		t.Errorf("nothing was switched off, so the report must not say so; got:\n%s", out)
	}
}

// TestMutationTest_AGoWorkAlreadySetIsNeverOverriddenOnAReRoot: the same precedence
// as the git child — a GOWORK in the daemon's own environment is an explicit choice.
func TestMutationTest_AGoWorkAlreadySetIsNeverOverriddenOnAReRoot(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	e.underGoWork(t)
	t.Setenv("GOWORK", "auto")

	out, err := e.run(t, e.mutant(e.file(e.wt), "42", "43"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	e.requireAllRanIn(t, e.wt)
	for _, got := range e.ranGOWORK(t) {
		if got != "auto" {
			t.Errorf("a re-rooted command saw GOWORK=%q, want the daemon's own \"auto\" left alone", got)
		}
	}
	if strings.Contains(out, "GOWORK=off") {
		t.Errorf("plumb did not switch anything off here, so the report must not say it did; got:\n%s", out)
	}
}
