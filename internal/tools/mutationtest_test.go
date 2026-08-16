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

// mutationtest_test.go covers mutation_test. The load-bearing cases are the two
// the tool exists for: a non-compiling mutant must be reported INVALID and
// never killed, and a mutant the tests do not catch must be reported SURVIVED
// rather than passing silently.

// --- harness -----------------------------------------------------------------

// mutationEnv is a disposable git repository with one source file, wired to a
// tool whose compile and test commands are scripts the test controls.
type mutationEnv struct {
	root          string
	file          string
	tool          *MutationTest
	compileScript string
	testScript    string
}

// newMutationEnv builds the repo. compileOK/testOK are the exit codes the two
// scripts return; a script that must react to the file's content is installed
// with installScript instead.
func newMutationEnv(t *testing.T, content string) *mutationEnv {
	t.Helper()
	root := t.TempDir()
	// Resolve symlinks so the workspace root matches the path safeWrite reports
	// (macOS /var → /private/var), which displayPath's filepath.Rel depends on.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	file := filepath.Join(root, "target.txt")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInit(t, root)

	env := &mutationEnv{
		root:          root,
		file:          file,
		compileScript: filepath.Join(root, "compile.sh"),
		testScript:    filepath.Join(root, "test.sh"),
	}
	env.installScript(t, env.compileScript, "exit 0")
	env.installScript(t, env.testScript, "exit 0")

	deps := WriteDeps{WorkspaceFn: func() string { return root }}
	env.tool = NewMutationTest(deps, func(slot, target string) (TaskCommand, error) {
		script := env.testScript
		if slot == "build" {
			script = env.compileScript
		}
		argv := []string{"/bin/sh", script}
		if target != "" {
			argv = append(argv, target)
		}
		return TaskCommand{Slot: slot, Steps: [][]string{argv}, Provenance: "default"}, nil
	})
	return env
}

// installScript writes a /bin/sh script body, committing it so the repo stays
// clean (mutation_test refuses a dirty file, and an uncommitted script would
// not itself block — but a clean tree keeps the fixture honest).
func (e *mutationEnv) installScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { //nolint:gosec // G306: a test fixture script must be executable
		t.Fatal(err)
	}
}

// failsOnlyWhenMutated installs a script that exits non-zero ONLY when the
// target file contains needle — that is, only once the mutant is on disk.
//
// Fixtures that fail unconditionally are refused by the baseline, and rightly:
// a command that fails before anything was mutated cannot tell "the mutant did
// it" from "it was always broken", which is the one distinction the tool sells.
// Writing them this way keeps the fixture honest about what a kill means.
func (e *mutationEnv) failsOnlyWhenMutated(t *testing.T, script, needle, msg string) {
	t.Helper()
	e.installScript(t, script,
		fmt.Sprintf(`grep -q '%s' "$(dirname "$0")/target.txt" && { echo '%s'; exit 1; }; exit 0`, needle, msg))
}

func gitInit(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
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

// commitAll re-commits the fixture so the tree is clean again after a test
// mutates a script.
func (e *mutationEnv) commitAll(t *testing.T) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = e.root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// run executes mutation_test with one mutant and returns the report.
func (e *mutationEnv) run(t *testing.T, old, newStr string) (string, error) {
	t.Helper()
	return e.runArgs(t, map[string]any{
		"mutants": []map[string]any{{"file_path": e.file, "old_string": old, "new_string": newStr}},
	})
}

func (e *mutationEnv) runArgs(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return e.tool.Execute(context.Background(), raw)
}

func (e *mutationEnv) content(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- the two classifications that matter -------------------------------------

// TestMutationTest_NonCompilingMutantIsInvalidNotKilled is the tool's reason to
// exist. The compile gate fails and the test command would ALSO have failed, so
// a harness that only looked at the test exit code would call this a kill. It
// must be reported invalid, and the word "killed" must not appear.
func TestMutationTest_NonCompilingMutantIsInvalidNotKilled(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	// Compile fails whenever the mutant is present; tests fail too, so only the
	// gate's precedence can produce the right answer.
	env.installScript(t, env.compileScript, `grep -q "BROKEN" "$(dirname "$0")/target.txt" && exit 2; exit 0`)
	env.installScript(t, env.testScript, `grep -q "BROKEN" "$(dirname "$0")/target.txt" && exit 1; exit 0`)
	env.commitAll(t)

	out, err := env.run(t, "42", "BROKEN")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "INVALID") {
		t.Errorf("a non-compiling mutant must be reported INVALID; got:\n%s", out)
	}
	if strings.Contains(out, "[1] KILLED") {
		t.Errorf("a non-compiling mutant must NEVER be reported as a kill; got:\n%s", out)
	}
	if !strings.Contains(out, reasonCompileFailed) {
		t.Errorf("report must state the mutant did not compile; got:\n%s", out)
	}
	if !strings.Contains(out, "summary: 0 killed") {
		t.Errorf("summary must count zero kills; got:\n%s", out)
	}
}

// TestMutationTest_SurvivingMutantIsReportedSurvived pins the other dangerous
// direction: tests pass with the mutant in place, and that must be surfaced as
// a SURVIVED finding rather than a quiet success.
func TestMutationTest_SurvivingMutantIsReportedSurvived(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	out, err := env.run(t, "42", "43") // both scripts exit 0
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "SURVIVED") {
		t.Fatalf("a mutant the tests do not catch must be reported SURVIVED; got:\n%s", out)
	}
	if !strings.Contains(out, "vacuous") {
		t.Errorf("a survival must say the assertions are vacuous; got:\n%s", out)
	}
	if !strings.Contains(out, "summary: 0 killed · 1 survived") {
		t.Errorf("summary must count the survival; got:\n%s", out)
	}
	// A survival must never read as an all-clear.
	if strings.Contains(out, "every mutant was killed") {
		t.Errorf("a run with a survivor must not print the all-killed line; got:\n%s", out)
	}
}

func TestMutationTest_KilledMutant(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	env.installScript(t, env.testScript, `grep -q "43" "$(dirname "$0")/target.txt" && { echo "FAIL: TestAnswer"; exit 1; }; exit 0`)
	env.commitAll(t)

	out, err := env.run(t, "42", "43")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "KILLED") {
		t.Fatalf("a mutant a test catches must be reported KILLED; got:\n%s", out)
	}
	if !strings.Contains(out, "summary: 1 killed") {
		t.Errorf("summary must count the kill; got:\n%s", out)
	}
	// The evidence excerpt proves the verdict came from a real failure.
	if !strings.Contains(out, "FAIL: TestAnswer") {
		t.Errorf("a kill must quote the failing output; got:\n%s", out)
	}
	if !strings.Contains(out, "every mutant was killed") {
		t.Errorf("an all-killed run should say so; got:\n%s", out)
	}
}

// TestMutationTest_UnappliedMutantIsInvalid covers the silent-sed case: the
// old_string matches nothing, so nothing was tested.
func TestMutationTest_UnappliedMutantIsInvalid(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	// The test command would fail the moment the mutation landed — so a harness
	// reading only the exit code would report a kill for a mutant that was never
	// applied. It never lands, so nothing runs and nothing may be claimed.
	env.failsOnlyWhenMutated(t, env.testScript, "zzz", "FAIL: TestAnswer")
	env.commitAll(t)

	out, err := env.run(t, "no-such-text", "zzz")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "INVALID") || !strings.Contains(out, reasonNotApplied) {
		t.Fatalf("an unmatched old_string must be INVALID/not-applied; got:\n%s", out)
	}
	if strings.Contains(out, "[1] KILLED") {
		t.Errorf("an unapplied mutant must never be a kill; got:\n%s", out)
	}
	if !strings.Contains(out, "nothing was compiled or tested") {
		t.Errorf("report must say nothing ran; got:\n%s", out)
	}
}

func TestMutationTest_AmbiguousMutantIsInvalid(t *testing.T) {
	env := newMutationEnv(t, "x = 1\nx = 1\n")
	out, err := env.run(t, "x = 1", "x = 2")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, reasonAmbiguous) {
		t.Fatalf("an old_string matching twice must be INVALID/ambiguous; got:\n%s", out)
	}
	if !strings.Contains(out, "(2 occurrences)") {
		t.Errorf("report must name the occurrence count; got:\n%s", out)
	}
}

// The restore guarantee and the safety preconditions live in
// mutationtest_safety_test.go, which shares this file's harness.

// --- the baseline refusal names WHICH cause it hit ----------------------------
//
// The baseline used to call stepOutcome.failed() — timedOut || startErr ||
// exitCode != 0 — and then assert ONE cause: "the test command ALREADY fails
// … Get the suite green first". So a command that timed out, and a command that
// could not be started at all, were both reported as a red suite with advice
// that fixes neither. classify() had always split these per mutant; only the
// per-run baseline collapsed them.
//
// These tests pin the split from the outside, through Execute, because that is
// where the agent reads the text.

// useArgv repoints the resolver at a fixed argv for one slot, so a test can
// stage a command that cannot start or never returns.
func (e *mutationEnv) useArgv(slot string, argv []string) {
	prev := e.tool.resolve
	e.tool.resolve = func(s, target string) (TaskCommand, error) {
		if s == slot {
			return TaskCommand{Slot: s, Steps: [][]string{argv}, Provenance: "default"}, nil
		}
		return prev(s, target)
	}
}

// baselineRefusal runs one mutant and requires the run to be REFUSED, returning
// the refusal text. A baseline failure must never produce a report.
func (e *mutationEnv) baselineRefusal(t *testing.T, args map[string]any) string {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	args["mutants"] = []map[string]any{{"file_path": e.file, "old_string": "42", "new_string": "43"}}
	out, err := e.runArgs(t, args)
	if err == nil {
		t.Fatalf("the run must be refused at the baseline; got a report:\n%s", out)
	}
	if got := e.content(t); !strings.Contains(got, "42") {
		t.Fatalf("the file was mutated despite a baseline refusal: %q", got)
	}
	return err.Error()
}

// TestBaseline_UnstartableCommandIsNotReportedAsARedSuite is the defect this
// change fixes. argv[0] does not exist, so plumb never ran anything — it has
// learned NOTHING about the workspace, and must not claim otherwise.
func TestBaseline_UnstartableCommandIsNotReportedAsARedSuite(t *testing.T) {
	for _, tc := range []struct {
		slot       string
		mustNotSay string
	}{
		{"build", "Fix the build first"},
		{"test", "Get the suite green first"},
	} {
		t.Run(tc.slot, func(t *testing.T) {
			env := newMutationEnv(t, "answer = 42\n")
			env.useArgv(tc.slot, []string{filepath.Join(env.root, "no-such-binary")})

			msg := env.baselineRefusal(t, nil)
			if !strings.Contains(msg, "could NOT BE STARTED") {
				t.Errorf("the refusal must say the command could not be started; got:\n%s", msg)
			}
			if strings.Contains(msg, tc.mustNotSay) {
				t.Errorf("a command that never ran must not be diagnosed as %q — that is a false diagnosis "+
					"with unactionable advice; got:\n%s", tc.mustNotSay, msg)
			}
			if strings.Contains(msg, "ALREADY fails") {
				t.Errorf("a command that never ran did not 'already fail'; got:\n%s", msg)
			}
		})
	}
}

// TestBaseline_TimeoutIsNotReportedAsARedSuite covers the other collapsed cause.
// The command ran and never returned, so the suite's colour is still unknown;
// the actionable knob is timeout_seconds, not the code.
func TestBaseline_TimeoutIsNotReportedAsARedSuite(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	env.installScript(t, env.testScript, "sleep 30")
	env.commitAll(t)

	msg := env.baselineRefusal(t, map[string]any{"timeout_seconds": 1})
	if !strings.Contains(msg, "TIMED OUT") {
		t.Errorf("the refusal must say the command timed out; got:\n%s", msg)
	}
	if !strings.Contains(msg, "timeout_seconds") {
		t.Errorf("the refusal must name the knob that fixes it (timeout_seconds); got:\n%s", msg)
	}
	if strings.Contains(msg, "ALREADY fails") || strings.Contains(msg, "Get the suite green first") {
		t.Errorf("a timeout returned no verdict, so the suite was not shown to be red; got:\n%s", msg)
	}
}

// TestBaseline_GenuinelyRedSuiteStillSaysSo is the control. Without it, a fix
// that simply deleted the red-suite wording would pass every test above.
func TestBaseline_GenuinelyRedSuiteStillSaysSo(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	env.installScript(t, env.testScript, "echo 'FAIL: pre-existing'; exit 1")
	env.commitAll(t)

	msg := env.baselineRefusal(t, nil)
	for _, want := range []string{"ALREADY fails", "Get the suite green first", "FAIL: pre-existing"} {
		if !strings.Contains(msg, want) {
			t.Errorf("a genuinely red suite must still be named as one (%q); got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "could NOT BE STARTED") || strings.Contains(msg, "TIMED OUT") {
		t.Errorf("a red suite must not be reported as a tooling or budget failure; got:\n%s", msg)
	}
}

// TestBaseline_NamesTheDirectoryItRanIn pins the other half of the false
// diagnosis. Task commands run from the WORKSPACE ROOT, which need not be a
// buildable directory: in a repository whose root holds only a go.work, with the
// module in a subdirectory, `go build ./...` exits 1 in milliseconds while the
// tree compiles perfectly — and the old text read "the workspace does not pass
// its compile gate … Fix the build first". Naming the cwd and the argv is what
// turns that from a wrong diagnosis into a solvable one.
//
// The directory assertion is deliberately NOT a bare Contains(msg, env.root).
// The fixture's script path lives inside env.root, so the argv alone satisfies
// that — a mutation run proved it: deleting the directory from the message left
// the bare check passing. It has to assert the cwd is named AS the cwd.
func TestBaseline_NamesTheDirectoryItRanIn(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	env.installScript(t, env.compileScript, "exit 1")
	env.commitAll(t)

	msg := env.baselineRefusal(t, nil)
	if !strings.Contains(msg, "in "+env.root+" ") {
		t.Errorf("the refusal must name the directory the command ran IN (%s), not merely mention the path "+
			"inside an argv; got:\n%s", env.root, msg)
	}
	if !strings.Contains(msg, "WORKSPACE ROOT") {
		t.Errorf("the refusal must explain WHY that directory is the cwd, or naming it teaches nothing; got:\n%s", msg)
	}
	if !strings.Contains(msg, env.compileScript) {
		t.Errorf("the refusal must name the argv it ran; got:\n%s", msg)
	}
}

// TestBaseline_OffersNoScopeAdviceItCannotHonour guards the second unactionable
// instruction that shared the message.
//
// "or scope the run with test_target" cannot be followed: the resolver either
// substitutes {target} away or refuses the command for lacking a placeholder, so
// by the time baseline runs there is no command a target could still be applied
// to. The advice therefore belongs in no branch, including the timeout one,
// where it used to appear even for the compile gate — which resolves without a
// target by construction.
func TestBaseline_OffersNoScopeAdviceItCannotHonour(t *testing.T) {
	const advice = "scope the run with test_target"

	t.Run("red suite", func(t *testing.T) {
		env := newMutationEnv(t, "answer = 42\n")
		env.installScript(t, env.testScript, "exit 1")
		env.commitAll(t)
		if msg := env.baselineRefusal(t, nil); strings.Contains(msg, advice) {
			t.Errorf("a resolved command cannot hold {target}, so this is advice the reader cannot follow; got:\n%s", msg)
		}
	})

	t.Run("compile gate timed out", func(t *testing.T) {
		env := newMutationEnv(t, "answer = 42\n")
		env.installScript(t, env.compileScript, "sleep 30")
		env.commitAll(t)
		msg := env.baselineRefusal(t, map[string]any{"timeout_seconds": 1})
		if !strings.Contains(msg, "TIMED OUT") {
			t.Fatalf("wanted the timeout branch; got:\n%s", msg)
		}
		if strings.Contains(msg, advice) {
			t.Errorf("the compile gate resolves WITHOUT a target, so scoping the test command cannot help it; got:\n%s", msg)
		}
	})
}

// TestBaseline_NamesTheStepThatFailed covers a composite command (verify = build
// then test): runStep stops at the first argv that fails, so the diagnostic must
// name THAT argv. Naming Steps[0] beside output the second step produced puts two
// contradictory facts in one message, and sends the reader to a command that
// passed.
func TestBaseline_NamesTheStepThatFailed(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	firstScript := filepath.Join(env.root, "first.sh")
	secondScript := filepath.Join(env.root, "second.sh")
	env.installScript(t, firstScript, "exit 0")
	env.installScript(t, secondScript, "echo 'the SECOND step is what failed'; exit 1")
	env.commitAll(t)

	prev := env.tool.resolve
	env.tool.resolve = func(slot, target string) (TaskCommand, error) {
		if slot == "test" {
			return TaskCommand{Slot: slot, Provenance: "default", Steps: [][]string{
				{"/bin/sh", firstScript},
				{"/bin/sh", secondScript},
			}}, nil
		}
		return prev(slot, target)
	}

	msg := env.baselineRefusal(t, nil)
	if !strings.Contains(msg, secondScript) {
		t.Errorf("the message must name the argv that failed (%s); got:\n%s", secondScript, msg)
	}
	if strings.Contains(msg, firstScript) {
		t.Errorf("it named %s, which PASSED — the reader would go and debug a working command; got:\n%s", firstScript, msg)
	}
}

// --- argument validation -----------------------------------------------------

func TestMutationTest_ArgValidation(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no mutants", map[string]any{"mutants": []map[string]any{}}, "mutants is required"},
		{
			"no-op mutant",
			map[string]any{"mutants": []map[string]any{{"file_path": "f", "old_string": "a", "new_string": "a"}}},
			"mutates nothing",
		},
		{
			"empty old_string",
			map[string]any{"mutants": []map[string]any{{"file_path": "f", "old_string": "", "new_string": "b"}}},
			"old_string is required",
		},
		{
			"unsafe target",
			map[string]any{
				"mutants":     []map[string]any{{"file_path": "f", "old_string": "a", "new_string": "b"}},
				"test_target": "a; rm -rf /",
			},
			"shell-safe",
		},
		{
			"bad slot",
			map[string]any{
				"mutants":   []map[string]any{{"file_path": "f", "old_string": "a", "new_string": "b"}},
				"test_task": "deploy",
			},
			"test_task must be one of",
		},
	}
	tool := NewMutationTest(WriteDeps{}, nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tool.Execute(context.Background(), raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestMutationTest_TooManyMutants(t *testing.T) {
	specs := make([]map[string]any, maxMutants+1)
	for i := range specs {
		specs[i] = map[string]any{"file_path": "f", "old_string": fmt.Sprintf("a%d", i), "new_string": "b"}
	}
	raw, err := json.Marshal(map[string]any{"mutants": specs})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewMutationTest(WriteDeps{}, nil).Execute(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "exceeds the limit") {
		t.Fatalf("want a batch-size refusal, got %v", err)
	}
}

func TestMutationTest_NoResolverIsRefused(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"mutants": []map[string]any{{"file_path": "f", "old_string": "a", "new_string": "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewMutationTest(WriteDeps{}, nil).Execute(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("want an unavailable-resolver refusal, got %v", err)
	}
}

// --- unit-level classification ------------------------------------------------

// TestClassify tables the verdict rule directly, including the shapes the
// end-to-end tests cannot easily stage (a compile timeout, a test timeout).
func TestClassify(t *testing.T) {
	ok := stepOutcome{ran: true, exitCode: 0}
	cases := []struct {
		name           string
		compile, tests stepOutcome
		want           MutationOutcome
		wantReason     string
	}{
		{"compiles, tests fail", ok, stepOutcome{ran: true, exitCode: 1}, MutationKilled, ""},
		{"compiles, tests pass", ok, ok, MutationSurvived, ""},
		{"does not compile", stepOutcome{ran: true, exitCode: 2}, stepOutcome{ran: true, exitCode: 1}, MutationInvalid, reasonCompileFailed},
		{"compile timed out", stepOutcome{ran: true, exitCode: -1, timedOut: true}, ok, MutationInvalid, reasonCompileTimeout},
		{"tests timed out", ok, stepOutcome{ran: true, exitCode: -1, timedOut: true}, MutationInvalid, reasonTestTimeout},
		// The two that read as an ordinary failure to anything counting only exit
		// codes. The TEST one is the dangerous half: without its own branch a
		// command that never launched is indistinguishable from a caught mutant.
		{"compile could not start", stepOutcome{ran: true, exitCode: -1, startErr: true}, ok, MutationInvalid, reasonCompileUnrunnable},
		{"tests could not start", ok, stepOutcome{ran: true, exitCode: -1, startErr: true}, MutationInvalid, reasonTestUnrunnable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r mutationResult
			r.classify(tc.compile, tc.tests)
			if r.outcome != tc.want {
				t.Errorf("outcome = %q, want %q", r.outcome, tc.want)
			}
			if r.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", r.reason, tc.wantReason)
			}
		})
	}
}

// TestClassify_TestFailureAfterAFailedCompileIsNeverAKill is the single
// assertion the whole compile gate exists for, stated on its own so a change
// that reorders the switch cannot pass silently.
func TestClassify_TestFailureAfterAFailedCompileIsNeverAKill(t *testing.T) {
	var r mutationResult
	r.classify(stepOutcome{ran: true, exitCode: 2}, stepOutcome{ran: true, exitCode: 1})
	if r.outcome == MutationKilled {
		t.Fatal("a failing test run after a FAILED compile must never be classified killed")
	}
	if r.outcome != MutationInvalid {
		t.Fatalf("outcome = %q, want invalid", r.outcome)
	}
}

// TestClassify_AnUnstartableTestCommandIsNeverAKill states the other half of
// the same rule on its own, because it is the one that was wrong.
//
// A command that could not be launched sets a non-zero exit code like any other
// failure, so classify's test switch called it a kill. A workspace whose test
// runner is simply not installed therefore reported EVERY mutant killed — the
// tool certifying assertions it never ran, which is exactly what it exists to
// prevent, reached from the tooling side rather than the mutant side.
func TestClassify_AnUnstartableTestCommandIsNeverAKill(t *testing.T) {
	var r mutationResult
	r.classify(
		stepOutcome{ran: true, exitCode: 0},
		stepOutcome{ran: true, exitCode: -1, startErr: true},
	)
	if r.outcome == MutationKilled {
		t.Fatal("a test command that never STARTED must never be classified killed")
	}
	if r.outcome != MutationInvalid || r.reason != reasonTestUnrunnable {
		t.Fatalf("outcome = %q reason = %q, want invalid/%s", r.outcome, r.reason, reasonTestUnrunnable)
	}
}

func TestMutateContent(t *testing.T) {
	cases := []struct {
		name, content, old, new string
		wantOut, wantReason     string
	}{
		{"applies once", "a b a c", "b", "z", "a z a c", ""},
		{"deletes", "keep drop", " drop", "", "keep", ""},
		{"missing", "a b", "zzz", "y", "", reasonNotApplied},
		{"crlf tolerated", "a\r\nb\r\n", "a\nb", "x\ny", "x\r\ny\r\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := mutateContent(tc.content, mutantSpec{Old: tc.old, New: tc.new})
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if got != tc.wantOut {
				t.Errorf("mutated = %q, want %q", got, tc.wantOut)
			}
		})
	}
}

func TestMutateContent_AmbiguousNamesTheCount(t *testing.T) {
	_, reason := mutateContent("x x x", mutantSpec{Old: "x", New: "y"})
	if !strings.Contains(reason, reasonAmbiguous) || !strings.Contains(reason, "3") {
		t.Fatalf("reason = %q, want the ambiguity reason naming 3 occurrences", reason)
	}
}

// --- restore failure escalation -----------------------------------------------

// TestRestoreFailed_EscalatesAndSavesASidecar pins the emergency path: the
// error names the file and its recovery, and the pre-mutation bytes are kept.
func TestRestoreFailed_EscalatesAndSavesASidecar(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "src.go")
	tool := NewMutationTest(WriteDeps{WorkspaceFn: func() string { return root }}, nil)
	tgt := mutationTarget{path: path, display: "src.go", original: []byte("original\n"), mode: 0o644}

	err := tool.restoreFailed(tgt, "disk on fire")
	if err == nil {
		t.Fatal("restoreFailed must return an error")
	}
	for _, want := range []string{"RESTORE FAILED", "src.go", "still MUTATED", "git checkout", "disk on fire"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("escalation must mention %q; got:\n%s", want, err)
		}
	}
	b, readErr := os.ReadFile(path + mutationRestoreSuffix)
	if readErr != nil {
		t.Fatalf("sidecar not written: %v", readErr)
	}
	if string(b) != "original\n" {
		t.Errorf("sidecar content = %q, want the pre-mutation bytes", b)
	}
}

// TestRestore_DetectsAnUnverifiableRestore mutates the snapshot's expected sha
// so the post-restore comparison must fail — proving the verification is a real
// check and not a formality.
func TestRestore_DetectsAnUnverifiableRestore(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "src.go")
	if err := os.WriteFile(path, []byte("whatever\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewMutationTest(WriteDeps{WorkspaceFn: func() string { return root }}, nil)
	tgt := mutationTarget{
		path: path, display: "src.go", original: []byte("original\n"), mode: 0o644,
		sha: "not-the-real-sha",
	}
	err := tool.restore(context.Background(), tgt)
	if err == nil {
		t.Fatal("a restore whose sha does not match the snapshot must be escalated")
	}
	if !strings.Contains(err.Error(), "does not match the pre-run snapshot") {
		t.Errorf("error should name the sha mismatch; got %v", err)
	}
}

// --- gitCleanliness ------------------------------------------------------------

func TestGitCleanliness(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Outside a repository: no safety net, and NOT reported dirty.
	//
	// The precondition is ESTABLISHED, not assumed. `make test` runs with
	// GOTMPDIR inside the checkout, and `go test` hands that to the test binary
	// as TMPDIR — so on CI t.TempDir() sits inside plumb's OWN repository and the
	// "loose" file has a .git above it after all. Assuming otherwise is why this
	// passed on a developer's machine (temp dir under /tmp) and failed on the
	// runner. Capping git's upward search one level above the directory makes the
	// case true wherever the temp dir happens to live.
	t.Run("outside a repository", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
		loose := filepath.Join(dir, "loose.txt")
		if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if inRepo, dirty := gitCleanliness(context.Background(), loose); inRepo || dirty {
			t.Errorf("a file outside a repo: inRepo=%v dirty=%v, want false/false", inRepo, dirty)
		}
	})

	env := newMutationEnv(t, "committed\n")
	if inRepo, dirty := gitCleanliness(context.Background(), env.file); !inRepo || dirty {
		t.Errorf("a committed file: inRepo=%v dirty=%v, want true/false", inRepo, dirty)
	}
	if err := os.WriteFile(env.file, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if inRepo, dirty := gitCleanliness(context.Background(), env.file); !inRepo || !dirty {
		t.Errorf("a modified file: inRepo=%v dirty=%v, want true/true", inRepo, dirty)
	}
	// Untracked counts as dirty: nothing at HEAD to recover.
	untracked := filepath.Join(env.root, "new.txt")
	if err := os.WriteFile(untracked, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if inRepo, dirty := gitCleanliness(context.Background(), untracked); !inRepo || !dirty {
		t.Errorf("an untracked file: inRepo=%v dirty=%v, want true/true", inRepo, dirty)
	}
}

// --- report shape --------------------------------------------------------------

func TestFormatMutationReport_Shape(t *testing.T) {
	args := mutationTestArgs{TestTask: "test", CompileTask: "build"}
	plan := mutationPlan{
		compile: TaskCommand{Steps: [][]string{{"go", "build", "./..."}}, Provenance: "default"},
		test:    TaskCommand{Steps: [][]string{{"go", "test", "./..."}}, Provenance: "default"},
		timeout: time.Minute,
	}
	results := []mutationResult{
		{
			spec: mutantSpec{Old: "a", New: "b", Label: "guard"}, display: "x.go", outcome: MutationKilled,
			compile: stepOutcome{ran: true}, test: stepOutcome{ran: true, exitCode: 1, output: "FAIL"},
		},
		{
			spec: mutantSpec{Old: "c", New: ""}, display: "y.go", outcome: MutationSurvived,
			compile: stepOutcome{ran: true}, test: stepOutcome{ran: true},
		},
		{spec: mutantSpec{Old: "d", New: "e"}, display: "z.go", outcome: MutationInvalid, reason: reasonNotApplied},
	}
	out := formatMutationReport(args, plan, []string{"z.go is not in a git repository"}, results)

	for _, want := range []string{
		"go build ./...", "go test ./...", "WHOLE suite",
		"no git safety net",
		"KILLED", "SURVIVED", "INVALID",
		"guard", "(deleted)",
		"summary: 1 killed · 1 survived · 1 invalid (of 3 mutants)",
		"restored and verified",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q; got:\n%s", want, out)
		}
	}
}

func TestTargetNote(t *testing.T) {
	if got := targetNote(""); !strings.Contains(got, "WHOLE suite") {
		t.Errorf("an unscoped run must warn it runs the whole suite; got %q", got)
	}
	if got := targetNote("./internal/tools"); !strings.Contains(got, "./internal/tools") {
		t.Errorf("a scoped run must name the target; got %q", got)
	}
}

func TestQuoteMutantText(t *testing.T) {
	if got := quoteMutantText(""); got != "(deleted)" {
		t.Errorf("an empty new_string must render as (deleted); got %q", got)
	}
	if got := quoteMutantText("a\nb"); !strings.Contains(got, "⏎") {
		t.Errorf("newlines must be flattened; got %q", got)
	}
	// Rune-safe truncation: slicing by byte would split the multi-byte runes.
	long := strings.Repeat("é", 200)
	got := quoteMutantText(long)
	if !strings.ContainsRune(got, '…') {
		t.Errorf("an over-long value must be truncated; got %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("truncation must not split a UTF-8 sequence; got %q", got)
	}
}

func TestExcerpt_TailNotHead(t *testing.T) {
	lines := make([]string, 0, 30)
	for i := range 30 {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	got := excerpt(strings.Join(lines, "\n"))
	if !strings.Contains(got, "line29") {
		t.Errorf("excerpt must keep the tail (the verdict); got:\n%s", got)
	}
	if strings.Contains(got, "line0\n") {
		t.Errorf("excerpt must drop the head when over the cap; got:\n%s", got)
	}
	if excerpt("   ") != "" {
		t.Error("blank output must produce no excerpt block")
	}
}
