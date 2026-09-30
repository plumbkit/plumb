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

// git_gowork_hook_test.go proves the per-repository GOWORK decision through the
// real path: a real git worktree, a real go.work, a real pre-commit hook running
// `go build ./...`, and the git tool's own Execute. Nothing here asserts what
// plumb intended to hand the child — the hooks write down the GOWORK the child
// actually saw, and the commit either lands (HEAD moves) or it does not.
//
// The scenario is the one the recommended isolation setup produces: a linked
// worktree of a Go repository, living inside a directory tree whose go.work
// does not list it. Go finds that go.work by walking up from the hook's working
// directory, and refuses every command with "directory prefix . does not
// contain modules listed in go.work". The failure is silent from the caller's
// side — `git log` still shows the previous commit — which is why the assertion
// is on HEAD and not on a return value.

// goWorkShape is where the repository being committed to sits relative to the
// enclosing go.work.
type goWorkShape string

const (
	// shapeInsideUsedDir: the worktree lives INSIDE a directory the go.work
	// `use`s — plumb's own field setup (.claude/worktrees/<name> under the main
	// checkout). Lexically covered, and still refused by Go, because the worktree
	// has its own go.mod and that module is not the one listed.
	shapeInsideUsedDir goWorkShape = "worktree inside a used directory"
	// shapeBesideUsedDir: the worktree is a sibling of the used directory.
	shapeBesideUsedDir goWorkShape = "worktree beside a used directory"
	// shapeWorktreeListed: the go.work lists the worktree itself. Go accepts
	// this, so GOWORK must be left alone — the over-application control.
	shapeWorktreeListed goWorkShape = "worktree listed in go.work"
	// shapeMainListed: an ordinary checkout that the go.work lists — the main
	// checkout of the repository, the configuration that legitimately NEEDS its
	// workspace and must never be switched off.
	shapeMainListed goWorkShape = "main checkout listed in go.work"
)

// goWorkFixture is a disposable tree: go.work at the top, the main checkout of a
// one-package Go module beneath it, optionally a linked worktree of that
// checkout, and a bare remote for push.
type goWorkFixture struct {
	tree string
	main string
	wt   string
	// repo is the directory the tool commits in: the worktree, or main for
	// shapeMainListed.
	repo string
	// seen is the directory the hooks record the GOWORK they observed into, one
	// file per hook.
	seen   string
	remote string
	// side and pick are commits reachable only from branches of the same names,
	// for rebase and cherry-pick to act on.
	sideSHA string
	pickSHA string
}

func newGoWorkFixture(t *testing.T, shape goWorkShape) *goWorkFixture {
	t.Helper()
	requireGit(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	// The daemon this models has no GOWORK of its own, and no go env file that sets
	// one: a developer's exported GOWORK or `go env` file would otherwise switch the
	// whole test to the "already chosen" branch, for plumb and for the hook's go alike.
	hermeticGoEnv(t)

	tree := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tree); err == nil {
		tree = resolved // macOS /var → /private/var; git reports the real path
	}
	f := &goWorkFixture{
		tree:   tree,
		main:   filepath.Join(tree, "main"),
		seen:   filepath.Join(tree, "seen"),
		remote: filepath.Join(tree, "remote.git"),
	}
	for _, d := range []string{f.main, f.seen} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gwWrite(t, f.main, "go.mod", "module example.com/gowork\n\ngo 1.21\n")
	gwWrite(t, f.main, "p.go", "package p\n")

	gwGit(t, f.main, "init", "-q", "-b", "main")
	gwGit(t, f.main, "config", "user.email", "t@example.com")
	gwGit(t, f.main, "config", "user.name", "t")
	gwGit(t, f.main, "config", "commit.gpgsign", "false")
	// Pin the hook directory: a developer's global core.hooksPath would otherwise
	// send git somewhere the fixture's hooks are not, and every "the hook saw X"
	// assertion below would be reading a hook that never ran.
	gwGit(t, f.main, "config", "core.hooksPath", filepath.Join(f.main, ".git", "hooks"))
	gwGit(t, f.main, "add", "-A")
	gwGit(t, f.main, "commit", "-q", "-m", "init")
	gwGit(t, tree, "init", "-q", "--bare", f.remote)
	gwGit(t, f.main, "remote", "add", "origin", f.remote)

	f.sideSHA = gwBranchWithCommit(t, f.main, "side", "side.txt")
	f.pickSHA = gwBranchWithCommit(t, f.main, "pick", "pick.txt")

	switch shape {
	case shapeInsideUsedDir:
		f.wt = filepath.Join(f.main, ".claude", "worktrees", "wt")
	case shapeBesideUsedDir, shapeWorktreeListed:
		f.wt = filepath.Join(tree, "wt")
	case shapeMainListed:
	}
	f.repo = f.main
	if f.wt != "" {
		gwGit(t, f.main, "worktree", "add", "-q", "-b", "feature", f.wt, "main")
		f.repo = f.wt
	}

	use := "./main"
	if shape == shapeWorktreeListed {
		use = "./wt" // a worktree shares main's module path, so both cannot be listed
	}
	gwWrite(t, tree, "go.work", "go 1.21\n\nuse "+use+"\n")
	f.installHooks(t)
	return f
}

// installHooks writes the real hooks last, after every setup commit, so none of
// the fixture's own git calls run them. Each records the GOWORK it was started
// with — "UNSET" when the variable is absent, which is not the same thing as
// empty — and the PWD it inherited, and pre-commit then does what the repository's
// real hook does.
func (f *goWorkFixture) installHooks(t *testing.T) {
	t.Helper()
	for _, name := range []string{"pre-commit", "post-commit", "pre-rebase", "pre-push"} {
		body := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"${GOWORK-UNSET}\" > %q\nprintf '%%s' \"${PWD-UNSET}\" > %q\n",
			filepath.Join(f.seen, name), filepath.Join(f.seen, name+".pwd"))
		if name == "pre-commit" {
			body += "go build ./...\n"
		}
		path := filepath.Join(f.main, ".git", "hooks", name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // G306: a git hook must be executable
			t.Fatal(err)
		}
	}
}

// sawGOWORK returns what the named hook recorded, failing the test when the hook
// never ran — a missing record must not be readable as "GOWORK was unset".
func (f *goWorkFixture) sawGOWORK(t *testing.T, hook string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.seen, hook))
	if err != nil {
		t.Fatalf("the %s hook never ran, so nothing can be said about its environment: %v", hook, err)
	}
	return string(b)
}

// commitThroughTool edits a tracked file in repo and commits it with the git
// tool, returning the tool's error and whether HEAD moved. HEAD is read with a
// plain git call, not from the tool's output, so a commit the tool reported and
// git never made cannot pass.
func (f *goWorkFixture) commitThroughTool(t *testing.T, tool *Git) (headMoved bool, err error) {
	t.Helper()
	before := gwGit(t, f.repo, "rev-parse", "HEAD")
	gwWrite(t, f.repo, "p.go", "package p\n\nvar Edited = true\n")
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{"p.go"}, "repo": f.repo}); err != nil {
		t.Fatalf("git add: %v", err)
	}
	_, err = callGit(t, tool, map[string]any{"subcommand": "commit", "message": "edit through the tool", "repo": f.repo})
	return gwGit(t, f.repo, "rev-parse", "HEAD") != before, err
}

func gwTool(env map[string]string) *Git {
	return NewGit(WriteDeps{}, func() GitPolicy {
		return GitPolicy{AllowWrites: true, AllowDestructive: true, AllowPush: true, Env: env}
	})
}

// --- the acceptance: the worktree commit lands with no shell fallback --------

// TestGit_CommitInAWorktreeOutsideTheEnclosingGoWorkLands is defect 1 of PLAN-442.
// The pre-commit hook is a real `go build ./...`; before the fix it died with
// "directory prefix . does not contain modules listed in go.work" and the only
// way through was `export GOWORK=off && git commit` in a shell.
func TestGit_CommitInAWorktreeOutsideTheEnclosingGoWorkLands(t *testing.T) {
	for _, shape := range []goWorkShape{shapeInsideUsedDir, shapeBesideUsedDir} {
		t.Run(string(shape), func(t *testing.T) {
			f := newGoWorkFixture(t, shape)
			moved, err := f.commitThroughTool(t, gwTool(nil))
			if err != nil || !moved {
				t.Fatalf("the commit did not land (HEAD moved=%v, err=%v) — the hook's `go build` found the enclosing go.work "+
					"and refused the worktree's module", moved, err)
			}
			if got := f.sawGOWORK(t, "pre-commit"); got != "off" {
				t.Errorf("the hook saw GOWORK=%q, want \"off\": the commit can only have landed by another route", got)
			}
		})
	}
}

// --- the controls, in the other direction ------------------------------------

// TestGit_GoWorkThatListsTheRepoIsLeftAlone is the over-application control: when
// the enclosing go.work DOES cover the repository, GOWORK=off would break the
// workspace the repository legitimately depends on. The commit lands with the
// variable still UNSET.
func TestGit_GoWorkThatListsTheRepoIsLeftAlone(t *testing.T) {
	for _, shape := range []goWorkShape{shapeWorktreeListed, shapeMainListed} {
		t.Run(string(shape), func(t *testing.T) {
			f := newGoWorkFixture(t, shape)
			moved, err := f.commitThroughTool(t, gwTool(nil))
			if err != nil || !moved {
				t.Fatalf("a repository the go.work lists must commit as it always did (HEAD moved=%v, err=%v)", moved, err)
			}
			if got := f.sawGOWORK(t, "pre-commit"); got != "UNSET" {
				t.Errorf("the hook saw GOWORK=%q, want it untouched (UNSET): plumb switched off a workspace that covers the repo", got)
			}
		})
	}
}

// explicitWorkFile writes a second go.work that DOES list the worktree, and
// returns its path. It is the value an explicit GOWORK is tested with: a value
// that still lets the hook's build pass, and that differs from the automatic
// "off", so an explicit setting that was overridden cannot pass by coincidence.
func (f *goWorkFixture) explicitWorkFile(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(f.tree, "alt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gwWrite(t, dir, "go.work", "go 1.21\n\nuse "+f.wt+"\n")
	return filepath.Join(dir, "go.work")
}

// TestGit_ExplicitGoWorkBeatsTheAutomaticOne pins the precedence: a [git] env
// GOWORK — the documented escape hatch — and a GOWORK already in the daemon's
// own environment both win over the per-repository decision, which never
// overrides one.
func TestGit_ExplicitGoWorkBeatsTheAutomaticOne(t *testing.T) {
	t.Run("[git] env entry", func(t *testing.T) {
		f := newGoWorkFixture(t, shapeInsideUsedDir)
		alt := f.explicitWorkFile(t)
		moved, err := f.commitThroughTool(t, gwTool(map[string]string{"GOWORK": alt}))
		if err != nil || !moved {
			t.Fatalf("commit with an explicit GOWORK (HEAD moved=%v, err=%v)", moved, err)
		}
		if got := f.sawGOWORK(t, "pre-commit"); got != alt {
			t.Errorf("the hook saw GOWORK=%q, want the configured %q — the automatic value overrode an explicit entry", got, alt)
		}
	})
	t.Run("inherited from the daemon", func(t *testing.T) {
		f := newGoWorkFixture(t, shapeInsideUsedDir)
		alt := f.explicitWorkFile(t)
		t.Setenv("GOWORK", alt)
		moved, err := f.commitThroughTool(t, gwTool(nil))
		if err != nil || !moved {
			t.Fatalf("commit with an inherited GOWORK (HEAD moved=%v, err=%v)", moved, err)
		}
		if got := f.sawGOWORK(t, "pre-commit"); got != alt {
			t.Errorf("the hook saw GOWORK=%q, want the inherited %q — an inherited GOWORK must never be overridden", got, alt)
		}
	})
	t.Run("an explicit empty entry is still explicit", func(t *testing.T) {
		f := newGoWorkFixture(t, shapeInsideUsedDir)
		// An empty GOWORK means "look for a go.work" to Go, so the hook fails here —
		// which is the point: the user asked for exactly that, and gets it.
		_, err := f.commitThroughTool(t, gwTool(map[string]string{"GOWORK": ""}))
		if err == nil {
			t.Fatal("an explicit empty GOWORK must be honoured, and Go then finds the enclosing go.work; the commit landing means plumb replaced it")
		}
		if got := f.sawGOWORK(t, "pre-commit"); got != "" {
			t.Errorf("the hook saw GOWORK=%q, want the explicit empty string", got)
		}
	})
}

// TestGit_TheHookSeesItsOwnDirectoryAsPWD: once plumb builds the child's
// environment — the automatic GOWORK=off, or any [git] env entry — os/exec stops
// setting PWD, and the hook used to inherit the DAEMON's (whatever directory it was
// started from). The hook must see the repository it runs in.
func TestGit_TheHookSeesItsOwnDirectoryAsPWD(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape goWorkShape
		env   map[string]string
	}{
		{"automatic GOWORK=off", shapeInsideUsedDir, nil},
		{"a [git] env entry, nothing switched off", shapeMainListed, map[string]string{"PLUMB_TEST_VAR": "x"}},
	} {
		env := tc.env
		t.Run(tc.name, func(t *testing.T) {
			f := newGoWorkFixture(t, tc.shape)
			t.Setenv("PWD", "/") // what an auto-spawned daemon has
			if moved, err := f.commitThroughTool(t, gwTool(env)); err != nil || !moved {
				t.Fatalf("commit (HEAD moved=%v, err=%v)", moved, err)
			}
			if got := f.sawGOWORK(t, "pre-commit.pwd"); got != f.repo {
				t.Errorf("the hook saw PWD=%q, want the repository %q", got, f.repo)
			}
		})
	}
}

// TestGit_AFailingHookSaysGOWORKWasSwitchedOff: when plumb switched the workspace
// off and the hook then fails, the failure must say so and name the setting that
// changes it — the generic "no plumb setting changes the outcome" would be false.
func TestGit_AFailingHookSaysGOWORKWasSwitchedOff(t *testing.T) {
	f := newGoWorkFixture(t, shapeInsideUsedDir)
	hook := filepath.Join(f.main, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'hook: needs a workspace module' >&2\nexit 1\n"), 0o755); err != nil { //nolint:gosec // G306: a git hook must be executable
		t.Fatal(err)
	}
	moved, err := f.commitThroughTool(t, gwTool(nil))
	if err == nil || moved {
		t.Fatalf("the hook exits 1, so the commit must fail (HEAD moved=%v, err=%v)", moved, err)
	}
	msg := err.Error()
	for _, want := range []string{"GOWORK=off", filepath.Join(f.tree, "go.work"), "[git] env GOWORK"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure must mention %q; got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "no plumb setting or flag changes the outcome") {
		t.Errorf("the failure claims no plumb setting matters, but [git] env GOWORK does; got:\n%s", msg)
	}

	// The control: the same failure where nothing was switched off keeps the
	// generic wording and says nothing about GOWORK.
	g := newGoWorkFixture(t, shapeMainListed)
	hook = filepath.Join(g.main, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil { //nolint:gosec // G306: a git hook must be executable
		t.Fatal(err)
	}
	if _, err := g.commitThroughTool(t, gwTool(nil)); err == nil || strings.Contains(err.Error(), "GOWORK") {
		t.Errorf("a failure with nothing switched off must not mention GOWORK; got %v", err)
	}
}

// TestGit_AGoEnvFileGOWORKIsHonoured: a GOWORK written in the go env file is go's
// own choice, exactly like an exported one, and plumb leaves it to go.
func TestGit_AGoEnvFileGOWORKIsHonoured(t *testing.T) {
	f := newGoWorkFixture(t, shapeInsideUsedDir)
	alt := f.explicitWorkFile(t)
	envFile := filepath.Join(f.tree, "goenv")
	gwWrite(t, f.tree, "goenv", "GOWORK="+alt+"\n")
	t.Setenv("GOENV", envFile)
	moved, err := f.commitThroughTool(t, gwTool(nil))
	if err != nil || !moved {
		t.Fatalf("the go env file's GOWORK lists the worktree, so the commit must land (HEAD moved=%v, err=%v)", moved, err)
	}
	if got := f.sawGOWORK(t, "pre-commit"); got != "UNSET" {
		t.Errorf("the hook's environment has GOWORK=%q; plumb must add nothing when the go env file already chose", got)
	}
}

// TestGit_ADeviceGoWorkDoesNotStallTheDaemon: a clone that commits `go.work ->
// /dev/zero` used to kill the shared daemon on its first git call (an unbounded
// read into memory). Every tier runs the decision, so a read-tier status is enough.
func TestGit_ADeviceGoWorkDoesNotStallTheDaemon(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("no /dev/zero")
	}
	f := newGoWorkFixture(t, shapeInsideUsedDir)
	work := filepath.Join(f.tree, "go.work")
	if err := os.Remove(work); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", work); err != nil {
		t.Skipf("symlink: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := callGit(t, gwTool(nil), map[string]any{"subcommand": "status", "repo": f.repo})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("git status beside a device go.work: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("git status is still running after 20s — the go.work decision is reading the device")
	}
}

// --- every hook-running verb goes through the same chokepoint ----------------

// TestGit_EveryHookRunningVerbGetsTheAutomaticGoWork walks the verbs that run a
// repository hook — commit, rebase, cherry-pick, push — through the tool in the
// failing shape and reads what each hook saw. execGitCmd is the single seam all
// of them share; this is the evidence that none of them reaches git around it.
// `merge` is absent on purpose: the tool classifies it tierReject, so it never
// spawns a child at all (a `pull` that merges is a network-tier verb and takes
// the same seam as push).
func TestGit_EveryHookRunningVerbGetsTheAutomaticGoWork(t *testing.T) {
	f := newGoWorkFixture(t, shapeInsideUsedDir)
	tool := gwTool(nil)
	if moved, err := f.commitThroughTool(t, tool); err != nil || !moved {
		t.Fatalf("setup commit (HEAD moved=%v, err=%v)", moved, err)
	}
	steps := []struct {
		verb, hook string
		args       map[string]any
	}{
		{"commit", "pre-commit", nil}, // already run above
		{"rebase", "pre-rebase", map[string]any{"subcommand": "rebase", "args": []string{"side"}, "confirm": true, "repo": f.repo}},
		{"cherry-pick", "post-commit", map[string]any{"subcommand": "cherry-pick", "args": []string{f.pickSHA}, "confirm": true, "repo": f.repo}},
		{"push", "pre-push", map[string]any{"subcommand": "push", "args": []string{"origin", "feature"}, "confirm": true, "repo": f.repo}},
	}
	for _, s := range steps {
		if s.args != nil {
			// post-commit also fired for the setup commit; clear it so the record
			// below is this verb's, not a stale one.
			_ = os.Remove(filepath.Join(f.seen, s.hook))
			if out, err := callGit(t, tool, s.args); err != nil {
				t.Fatalf("git %s through the tool: %v\n%s", s.verb, err, out)
			}
		}
		if got := f.sawGOWORK(t, s.hook); got != "off" {
			t.Errorf("git %s: the %s hook saw GOWORK=%q, want \"off\" — this verb reached git without the per-repository decision",
				s.verb, s.hook, got)
		}
	}
}

// --- helpers -----------------------------------------------------------------

// unsetEnvForTest removes key from the process environment for the test and
// restores it afterwards. t.Setenv(key, "") alone is not the same thing: it leaves
// the variable PRESENT, which is exactly the state the code under test treats as
// "an explicit choice". So the Setenv only registers the restore of whatever was
// there — including "not set" — and the Unsetenv that follows is what the test
// relies on.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func gwWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // G306: test fixture file
		t.Fatal(err)
	}
}

// gwGit runs git in dir for fixture setup, failing the test on error, and returns
// its trimmed stdout.
func gwGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gwBranchWithCommit creates branch off main carrying one new file, returns the
// commit's SHA and leaves main checked out.
func gwBranchWithCommit(t *testing.T, repo, branch, file string) string {
	t.Helper()
	gwGit(t, repo, "switch", "-q", "-c", branch, "main")
	gwWrite(t, repo, file, branch+"\n")
	gwGit(t, repo, "add", file)
	gwGit(t, repo, "commit", "-q", "-m", branch)
	sha := gwGit(t, repo, "rev-parse", "HEAD")
	gwGit(t, repo, "switch", "-q", "main")
	return sha
}

// TestRunTask_InAShadowedWorktreeBuilds: the same decision covers the stored task
// commands. A session pinned to the worktree ran `go build ./...` under the
// enclosing go.work and was refused; it now builds, and says GOWORK was switched
// off. The main checkout, which the go.work lists, is untouched.
func TestRunTask_InAShadowedWorktreeBuilds(t *testing.T) {
	f := newGoWorkFixture(t, shapeInsideUsedDir)
	for _, tc := range []struct {
		dir      string
		wantNote bool
	}{{f.wt, true}, {f.main, false}} {
		dir := tc.dir
		tool := NewTasks(WriteDeps{WorkspaceFn: func(context.Context) string { return dir }},
			func(_ context.Context, req TaskRequest) (TaskCommand, error) {
				slot := req.Slot
				return TaskCommand{Slot: slot, Steps: [][]string{{"go", "build", "./..."}}, Provenance: "default"}, nil
			})
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"slot":"build"}`))
		if err != nil || !strings.Contains(out, "→ ok") {
			t.Fatalf("run_task build in %s: err=%v\n%s", dir, err, out)
		}
		if got := strings.Contains(out, "GOWORK=off"); got != tc.wantNote {
			t.Errorf("run_task in %s: GOWORK=off note present=%v, want %v\n%s", dir, got, tc.wantNote, out)
		}
	}
}
