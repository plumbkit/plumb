package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// TestRunTaskArgv_TaskEnvWinsAndExplicitGoWorkIsHonoured pins #537's ordering. A
// task env entry replaces the inherited value of its name, and it is in place
// before the automatic GOWORK decision: in the worktree layout where plumb would
// otherwise switch GOWORK off, an explicit GOWORK from the task env is used as is,
// exactly as an inherited one already is. PWD still names the directory.
func TestRunTaskArgv_TaskEnvWinsAndExplicitGoWorkIsHonoured(t *testing.T) {
	hermeticGoEnv(t)
	t.Setenv("PLUMB_RUNARGV_PROBE", "inherited")
	t.Setenv("PWD", "/")
	root := goWorkLayout(t, map[string]string{
		"go.work":     "go 1.21\n\nuse ./main\n",
		"main/go.mod": goModMain,
		"wt/go.mod":   goModMain,
	})
	wt := filepath.Join(root, "wt")
	probe := []string{"sh", "-c", `printf '%s|%s|%s' "${GOWORK-UNSET}" "$PWD" "${PLUMB_RUNARGV_PROBE-UNSET}"`}
	ctx := context.Background()

	res, err := RunTaskArgv(ctx, wt, probe, []string{"GOWORK=auto", "PLUMB_RUNARGV_PROBE=task"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := "auto|" + wt + "|task"; res.Stdout != want || res.GoWorkOff != "" {
		t.Errorf("with a task env the command saw %q (GoWorkOff %q), want %q with GOWORK left alone", res.Stdout, res.GoWorkOff, want)
	}

	// Positive control: the same directory without the env gets the automatic
	// switch, so the assertion above is about the env and not about the layout.
	res, err = RunTaskArgv(ctx, wt, probe, nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := "off|" + wt + "|inherited"; res.Stdout != want {
		t.Errorf("without a task env the command saw %q, want %q", res.Stdout, want)
	}
}

// TestPrepareTaskEnv_ExpandsAndCreatesOnlyInsideTheWorkspace pins the expansion
// of both placeholders and the one directory plumb creates: a temp-dir variable
// naming a directory inside the workspace (what `make test`'s $(TESTCACHE)
// prerequisite does for plumb), never one outside it.
func TestPrepareTaskEnv_ExpandsAndCreatesOnlyInsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "module")
	outside := filepath.Join(t.TempDir(), "elsewhere")

	got := PrepareTaskEnv([]string{
		"GOTMPDIR={workspace}/.testcache",
		"OUT={working_dir}/out",
		"TMPDIR=" + outside,
	}, root, dir)

	want := []string{"GOTMPDIR=" + root + "/.testcache", "OUT=" + dir + "/out", "TMPDIR=" + outside}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("PrepareTaskEnv = %q, want %q", got, want)
	}
	if info, err := os.Stat(filepath.Join(root, ".testcache")); err != nil || !info.IsDir() {
		t.Errorf("GOTMPDIR inside the workspace was not created: %v", err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("a temp dir OUTSIDE the workspace must not be created, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
		t.Errorf("only temp-dir variables are created, but OUT's directory exists (err %v)", err)
	}

	// With no root there is no "inside": PathWithinWorkspace("", v) is true for
	// any v, and nothing may be created on that answer.
	noRoot := filepath.Join(t.TempDir(), "would-be-created")
	PrepareTaskEnv([]string{"GOTMPDIR=" + noRoot}, "", "")
	if _, err := os.Stat(noRoot); !os.IsNotExist(err) {
		t.Errorf("with an empty root a temp dir must not be created, stat err = %v", err)
	}
}

func TestDescribeTaskEnv_WithholdsCredentials(t *testing.T) {
	got := describeTaskEnv([]string{"GOTMPDIR=/w/.testcache", "GITHUB_TOKEN=opaque-value-123", "NPM_PASSWORD=hunter22"})
	if !strings.Contains(got, "GOTMPDIR=/w/.testcache") {
		t.Errorf("an ordinary value must be shown: %q", got)
	}
	for _, secret := range []string{"opaque-value-123", "hunter22"} {
		if strings.Contains(got, secret) {
			t.Errorf("a credential value leaked into the report: %q", got)
		}
	}
	if !strings.Contains(got, "GITHUB_TOKEN=[REDACTED]") {
		t.Errorf("a withheld variable must still be NAMED: %q", got)
	}
}

// TestRunTask_AppliesAndReportsTaskEnv is the end-to-end half: the resolver's env
// reaches the process with {workspace} expanded against the resolver's root, the
// temp directory exists before the command runs, and the report names the env.
func TestRunTask_AppliesAndReportsTaskEnv(t *testing.T) {
	root := t.TempDir()
	tool := NewTasks(WriteDeps{WorkspaceFn: func(context.Context) string { return root }},
		func(_ context.Context, req TaskRequest) (TaskCommand, error) {
			return TaskCommand{
				Slot: req.Slot, Provenance: "project", Root: root,
				Env:   []string{"GOTMPDIR={workspace}/.testcache"},
				Steps: [][]string{{"sh", "-c", `test -d "$GOTMPDIR" && printf 'saw:%s' "$GOTMPDIR"`}},
			}, nil
		})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"slot":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := root + "/.testcache"
	if !strings.Contains(out, "env: GOTMPDIR="+want) {
		t.Errorf("the report must list the applied env; got:\n%s", out)
	}
	if !strings.Contains(out, "saw:"+want) || !strings.Contains(out, "→ ok") {
		t.Errorf("the command did not see an existing GOTMPDIR=%s; got:\n%s", want, out)
	}
}

// TestRunTask_RunAndVerboseReachTheResolver pins #538's plumbing: both reach the
// resolver, which is where the placeholders are filled.
func TestRunTask_RunAndVerboseReachTheResolver(t *testing.T) {
	var got TaskRequest
	tool := NewTasks(WriteDeps{}, func(_ context.Context, req TaskRequest) (TaskCommand, error) {
		got = req
		return TaskCommand{Slot: req.Slot, Steps: [][]string{{"true"}}}, nil
	})
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"slot":"test","run":"TestA|TestB","verbose":true}`)); err != nil {
		t.Fatal(err)
	}
	if got.Run != "TestA|TestB" || !got.Verbose {
		t.Errorf("resolver saw %+v, want Run TestA|TestB and Verbose", got)
	}
}

// TestRunFilter_FlagOverAlternationIsRefused: go test -run compiles each
// top-level | alternative, and each top-level / subtest level, on its own, so
// `(?i)write|delete|copy` is case-insensitive for `write` only. Reproduced in
// PLAN-450: that filter ran 159 of the 215 tests -list selected and a harness
// reported 17/17 SURVIVED, every one false. The suggested spelling must itself
// be accepted, and splits Go does not make (inside a group or a nested class)
// must not be refused.
func TestRunFilter_FlagOverAlternationIsRefused(t *testing.T) {
	for bad, fixed := range map[string]string{
		"(?i)write|delete|copy": "(?i)write|(?i)delete|(?i)copy",
		"(?i)a|b":               "(?i)a|(?i)b",
		"(?is)Test(A|B)|TestC":  "(?is)Test(A|B)|(?is)TestC",
		"(?i)[|]x|y":            "(?i)[|]x|(?i)y",
		"(?i)TestFoo/bar":       "(?i)TestFoo/(?i)bar",
		"(?-i)a|b":              "(?-i)a|(?-i)b",
		"(?i-s)a|(?i-s)b|c":     "(?i-s)a|(?i-s)b|(?i-s)c",
	} {
		err := validateRunFilter("run", bad)
		if err == nil {
			t.Errorf("%q must be refused", bad)
			continue
		}
		if !strings.Contains(err.Error(), "Put the flag on every part: "+fixed) {
			t.Errorf("%q: the refusal must suggest %q: %v", bad, fixed, err)
		}
		if verr := validateRunFilter("run", fixed); verr != nil {
			t.Errorf("the suggested %q must itself be accepted: %v", fixed, verr)
		}
	}
	if err := validateRunFilter("run", "(?i)write|delete"); err == nil || !strings.Contains(err.Error(), "or group the alternatives: (?i)(write|delete)") {
		t.Errorf("with no / the refusal must also offer the grouped spelling: %v", err)
	}
	if err := validateRunFilter("run", "(?i)TestFoo/bar"); err == nil || strings.Contains(err.Error(), "group the alternatives") {
		t.Errorf("grouping across a / would stop subtest matching, so it must not be offered: %v", err)
	}
	for _, ok := range []string{
		"(?i)(write|delete|copy)", "(?i)TestWrite", "(?i)Test(A|B)", "(?i)[a|b]x", "TestA|TestB", "TestFoo/bar",
		"(?i)[[:alpha:]|x]y", "(?i)[[]|a", "(?i:a|b)",
		"(?i)TestFoo/", "(?i)TestA|(?i:b)", "(?i)TestA|(?s)b",
	} {
		if err := validateRunFilter("run", ok); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
}

// TestRunFilter_AdviceFitsTheLengthLimit: putting the flag on every part
// lengthens a filter, and a suggestion past the 256-character limit would only
// be refused in its turn. Only spellings the validator accepts are offered.
func TestRunFilter_AdviceFitsTheLengthLimit(t *testing.T) {
	parts := func(p string, n int) string { return strings.TrimSuffix(strings.Repeat(p+"|", n), "|") }

	// 60 alternatives: the run fits (183 chars), the per-part form does not
	// (419), the grouped form does (185).
	run := "(?i)" + parts("ab", 60)
	err := validateRunFilter("run", run)
	if err == nil {
		t.Fatal("must be refused")
	}
	if msg := err.Error(); strings.Contains(msg, "put the flag on every part:") || strings.Contains(msg, "Put the flag on every part:") {
		t.Errorf("the per-part spelling is over the limit and must not be offered:\n%v", err)
	}
	if want := "Group the alternatives: (?i)(" + parts("ab", 60) + ")"; !strings.Contains(err.Error(), want) {
		t.Errorf("the grouped spelling fits and must be offered:\n%v", err)
	}

	// With a / there is no grouped form either, so neither spelling fits.
	run = "(?i)" + parts("ab/cd", 40)
	if err = validateRunFilter("run", run); err == nil || !strings.Contains(err.Error(), "shorten or split the filter") {
		t.Errorf("with no spelling that fits, the refusal must say to shorten or split it: %v", err)
	}

	// 84 alternatives, no /: the run is 255 characters, so even the grouped
	// form (257) is over the limit and must not be offered.
	run = "(?i)" + parts("ab", 84)
	err = validateRunFilter("run", run)
	if err == nil || strings.Contains(err.Error(), "roup the alternatives") || !strings.Contains(err.Error(), "shorten or split the filter") {
		t.Errorf("a grouped spelling over the limit must not be offered: %v", err)
	}
}

func TestRunFilter_Validation(t *testing.T) {
	for _, ok := range []string{"TestA|TestB", "^TestX$", "TestFoo/sub_case", "slow and not db", "Test(A|B)[0-9]+.*", "a@b"} {
		if err := validateRunFilter("run", ok); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"-exec=/tmp/x", "-v", " TestA", "@args.txt", "a;b", "a>b", "a\nb", "a&b", strings.Repeat("a", 257)} {
		if err := validateRunFilter("run", bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	tool := NewTasks(WriteDeps{}, func(context.Context, TaskRequest) (TaskCommand, error) {
		t.Fatal("a refused run filter must not reach the resolver")
		return TaskCommand{}, nil
	})
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"slot":"test","run":"-exec=/tmp/x"}`)); err == nil {
		t.Error("run_task accepted a run filter that starts with -")
	}
}

// TestMutationTest_TestRunReachesTheTestCommandOnly pins test_run: it scopes the
// TEST command, and the compile gate stays unscoped (a whole-module compile is
// the stronger check).
func TestMutationTest_TestRunReachesTheTestCommandOnly(t *testing.T) {
	seen := map[string]TaskRequest{}
	mt := NewMutationTest(WriteDeps{}, func(_ context.Context, req TaskRequest) (TaskCommand, error) {
		seen[req.Slot] = req
		return TaskCommand{Slot: req.Slot, Steps: [][]string{{"true"}}}, nil
	})
	args := mutationTestArgs{TestRun: "TestKill"}.withDefaults()
	plan, err := mt.resolvePlan(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if seen["test"].Run != "TestKill" || seen["build"].Run != "" {
		t.Errorf("resolver saw test %+v, build %+v; want the filter on the test command only", seen["test"], seen["build"])
	}
	if !strings.Contains(targetNote(plan.target, plan.test), "TestKill") {
		t.Errorf("the report must say the run was scoped by the filter, got %q", targetNote(plan.target, plan.test))
	}
	bad := mutationTestArgs{Mutants: []mutantSpec{{Path: "f", Old: "a", New: "b"}}, TestRun: "-exec=x"}.withDefaults()
	if err := bad.validate(); err == nil {
		t.Error("mutation_test accepted a test_run that starts with -")
	}
}

// TestRerootedRoot_FollowsTheCommand pins that {workspace} moves with a command
// mutation_test re-roots, so GOTMPDIR lands in the work-tree the tests run in.
func TestRerootedRoot_FollowsTheCommand(t *testing.T) {
	from, dest := t.TempDir(), t.TempDir()
	if got := rerootedRoot(from, from, dest); got != dest {
		t.Errorf("root at the tree top moved to %q, want %q", got, dest)
	}
	sub := filepath.Join(from, "a", "b")
	if got := rerootedRoot(sub, from, dest); got != filepath.Join(dest, "a", "b") {
		t.Errorf("root below the tree top moved to %q", got)
	}
	above := filepath.Dir(from)
	if got := rerootedRoot(above, from, dest); got != above {
		t.Errorf("a root outside the tree being left must stay, got %q", got)
	}
}

func TestTaskEnvPlaceholders_MatchConfig(t *testing.T) {
	if taskEnvWorkspace != config.TaskEnvWorkspace || taskEnvWorkingDir != config.TaskEnvWorkingDir {
		t.Errorf("tools expands %s/%s but config validates %s/%s",
			taskEnvWorkspace, taskEnvWorkingDir, config.TaskEnvWorkspace, config.TaskEnvWorkingDir)
	}
}

// TestMutationTest_AReRootMovesTheEnvWorkspace is the end-to-end half of
// rerootedRoot: a mutant in a linked worktree re-roots the commands there, and
// {workspace} in their env must name that worktree, not the checkout the
// commands were resolved for — or GOTMPDIR would point back into the tree the
// tests are no longer running in.
func TestMutationTest_AReRootMovesTheEnvWorkspace(t *testing.T) {
	e := newWorktreeEnv(t, "", false, false)
	probe := filepath.Join(t.TempDir(), "probe")
	deps := WriteDeps{WorkspaceFn: func(context.Context) string { return e.root }}
	e.tool = NewMutationTest(deps, func(_ context.Context, req TaskRequest) (TaskCommand, error) {
		argv := []string{"/bin/sh", "-c", `echo "$PLUMB_ROOT_PROBE" >> ` + shellQuote(probe)}
		return TaskCommand{
			Slot: req.Slot, Steps: [][]string{argv}, Provenance: "default",
			Root: e.root, Env: []string{"PLUMB_ROOT_PROBE={workspace}"},
		}, nil
	})
	if _, err := e.run(t, e.mutant(e.file(e.wt), "42", "43")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	seen := strings.Fields(string(data))
	if len(seen) == 0 {
		t.Fatal("no command ran, so nothing was checked")
	}
	for _, got := range seen {
		if got != e.wt {
			t.Errorf("{workspace} expanded to %q in a run re-rooted into %q", got, e.wt)
		}
	}
}
