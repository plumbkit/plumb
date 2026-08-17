package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// mutationtest_run.go is mutation_test's execution half: the mutate → compile →
// test → restore cycle for a single mutant, and the restore guarantee that
// wraps it.
//
// RESTORE IS THE INVARIANT. Every exit path from runOne — a clean pass, a test
// failure, a compile failure, a timeout, a panic unwinding the stack, a
// cancelled context — runs the same deferred restore against an in-memory
// snapshot taken before the file was touched, while the per-path lock is still
// held. The restore is then VERIFIED by SHA-256 against that snapshot; an
// unverified restore is escalated, never assumed.

// mutationRestoreSuffix names the emergency sidecar written only when
// restoration itself fails, so the pre-mutation bytes survive even without git.
const mutationRestoreSuffix = ".plumb-mutation-backup"

// stepOutcome is the bounded result of one compile or test invocation.
//
// startErr is kept SEPARATE from a non-zero exitCode, and the distinction is
// load-bearing rather than cosmetic. "The test binary is not on PATH" and "the
// tests ran and failed" are the same exit code to a caller that only looks at a
// number, and collapsing them is precisely the false kill this tool exists to
// prevent — reached from the tooling side instead of the mutant side.
type stepOutcome struct {
	ran       bool
	exitCode  int
	timedOut  bool
	cancelled bool
	startErr  bool
	elapsed   time.Duration
	output    string
	// step indexes the argv this outcome came from. A composite command (verify
	// = build then test) stops at its first failure, so the argv worth naming in
	// a diagnostic is that one — not Steps[0], which may have passed.
	step int
}

// failed reports whether this step did anything other than succeed, by any
// route. Used to decide whether a later step is worth running at all.
func (s stepOutcome) failed() bool {
	return s.failure() != stepOK
}

// stepFailure names WHY a step did not succeed. The four causes are not
// interchangeable and must never be collapsed into "it failed":
//
//	"the tests ran and something is red"      → the workspace has a problem
//	"the command never started"               → the TOOLING has a problem
//	"the command ran out of time"             → the BUDGET has a problem
//	"the run was cancelled"                   → the REQUEST went away, no verdict
//
// Each needs a different thing done about it, so a message that asserts the
// first when the truth is one of the others sends the reader to fix something
// that was never broken. That is the same false-attribution defect the compile
// gate exists to prevent, arrived at from the diagnostics side.
//
// Shared by classify (per mutant) and baseline (per run) so the two cannot
// drift: classify already split these correctly while baseline collapsed them,
// which is exactly the divergence one enum in one place makes impossible.
type stepFailure int

const (
	stepOK stepFailure = iota
	stepUnrunnable
	stepTimedOut
	stepCancelled
	stepExited
)

// failure classifies this outcome. What is load-bearing is that startErr,
// timedOut and cancelled are all tested BEFORE exitCode: a command that could
// not be started carries exitCode -1, a timeout carries one too, and a
// cancelled command carries one too, so testing exitCode first would swallow
// all three causes that are not about the workspace. Their order relative to
// EACH OTHER is not — runStep breaks before it can set more than one.
func (s stepOutcome) failure() stepFailure {
	switch {
	case s.startErr:
		return stepUnrunnable
	case s.timedOut:
		return stepTimedOut
	case s.cancelled:
		return stepCancelled
	case s.exitCode != 0:
		return stepExited
	default:
		return stepOK
	}
}

// mutationResult is one mutant's classified verdict.
type mutationResult struct {
	spec    mutantSpec
	display string
	outcome MutationOutcome
	reason  string // non-empty only for MutationInvalid
	compile stepOutcome
	test    stepOutcome
}

// runAll runs each mutant in turn. It stops at the first RESTORE failure —
// that is an emergency, not a result, and continuing would mutate a second file
// while the first is still broken on disk. Results gathered so far are returned
// alongside the error so the caller can still report them.
func (t *MutationTest) runAll(ctx context.Context, targets []mutationTarget, plan mutationPlan) ([]mutationResult, error) {
	results := make([]mutationResult, 0, len(targets))
	for _, tgt := range targets {
		res, restoreErr := t.runOne(ctx, tgt, plan)
		results = append(results, res)
		if restoreErr != nil {
			return results, restoreErr
		}
	}
	return results, nil
}

// runOne applies one mutant, classifies it, and restores the file.
//
// The named restoreErr return is set by the deferred restore, so restoration
// happens on every path out of this function including a panic. The per-path
// lock is acquired BEFORE the snapshot is committed to and released only after
// the restore, so no other plumb write tool can observe or overwrite the
// mutated state.
func (t *MutationTest) runOne(ctx context.Context, tgt mutationTarget, plan mutationPlan) (res mutationResult, restoreErr error) {
	res = mutationResult{spec: tgt.spec, display: tgt.display}

	release := lockPath(tgt.path)
	defer release()

	mutated, reason := mutateContent(string(tgt.original), tgt.spec)
	if reason != "" {
		res.outcome, res.reason = MutationInvalid, reason
		return res, nil
	}
	if _, err := safeWrite(tgt.path, []byte(mutated), tgt.mode); err != nil {
		res.outcome = MutationInvalid
		res.reason = fmt.Sprintf("could not write the mutant: %v", err)
		return res, nil
	}
	// From here the file on disk is mutated: nothing may return without the
	// deferred restore running.
	defer func() { restoreErr = t.restore(ctx, tgt) }()
	t.announce(ctx, tgt.path)

	// Sequenced, not evaluated as two arguments: a mutant that does not compile
	// has nothing to learn from running the suite against a tree that will not
	// build, and doing so burns a full test timeout per broken mutant.
	compile := t.runStep(ctx, plan.compile, plan.timeout)
	var test stepOutcome
	if !compile.failed() {
		test = t.runStep(ctx, plan.test, plan.timeout)
	}
	res.classify(compile, test)
	return res, nil
}

// classify turns the two step outcomes into a verdict. It is the whole point of
// the tool, so it is one small readable function with no side effects.
//
// A test failure is a kill ONLY when the compile gate passed AND the test
// command actually ran. Every other shape — a mutant that did not compile, a
// compile that timed out, a test run that timed out, a step that was cancelled,
// or a command that could not be started at all — is invalid, because none of
// them distinguishes "the
// assertion caught the change" from "the toolchain never got far enough to try".
//
// The startErr cases are not hypothetical bookkeeping. A test command whose
// binary is missing exits non-zero in every sense a naive check can see, so
// without this branch a workspace with (say) an uninstalled test runner reports
// EVERY mutant killed — the tool certifying assertions it never executed, which
// is the exact failure it was built to stop.
func (r *mutationResult) classify(compile, test stepOutcome) {
	r.compile = compile
	switch compile.failure() {
	case stepUnrunnable:
		r.outcome, r.reason = MutationInvalid, reasonCompileUnrunnable
		return
	case stepTimedOut:
		r.outcome, r.reason = MutationInvalid, reasonCompileTimeout
		return
	case stepCancelled:
		r.outcome, r.reason = MutationInvalid, reasonCompileCancelled
		return
	case stepExited:
		r.outcome, r.reason = MutationInvalid, reasonCompileFailed
		return
	case stepOK:
	}
	r.test = test
	switch test.failure() {
	case stepUnrunnable:
		r.outcome, r.reason = MutationInvalid, reasonTestUnrunnable
	case stepTimedOut:
		r.outcome, r.reason = MutationInvalid, reasonTestTimeout
	case stepCancelled:
		r.outcome, r.reason = MutationInvalid, reasonTestCancelled
	case stepExited:
		r.outcome = MutationKilled
	case stepOK:
		r.outcome = MutationSurvived
	}
}

// mutateContent applies the spec to content, returning the mutated text or the
// invalid-reason explaining why it did not apply. The match must be EXACTLY
// once: zero occurrences means the caller's old_string is wrong (the silent
// `sed` no-op that makes a hand-rolled harness report a phantom kill), and
// several means the caller cannot know which line was tested.
func mutateContent(content string, spec mutantSpec) (mutated, reason string) {
	old := matchLineEndings(spec.Old, content)
	newStr := matchLineEndings(spec.New, content)
	switch n := strings.Count(content, old); {
	case n == 0:
		return "", reasonNotApplied
	case n > 1:
		return "", fmt.Sprintf("%s (%d occurrences)", reasonAmbiguous, n)
	}
	return strings.Replace(content, old, newStr, 1), ""
}

// runStep executes every argv of a resolved task command in sequence, stopping
// at the first non-zero exit. A command that cannot be started at all (argv[0]
// not on PATH, or any other failure to launch) sets startErr, which classify
// turns into an invalid verdict for either step — the flag exists because a
// bare non-zero exitCode would read as a kill in the test slot.
func (t *MutationTest) runStep(ctx context.Context, cmd TaskCommand, timeout time.Duration) stepOutcome {
	var out stepOutcome
	out.ran = true
	start := time.Now()
	// The resolver's directory when it has one — the workspace root is only the
	// fallback. In a holder repository (go.work at the top, the module below) the
	// root is where every command fails, so running the compile gate there made
	// the whole tool unusable in exactly the repositories that need it.
	ws := cmd.WorkingDir
	if ws == "" && t.deps.WorkspaceFn != nil {
		ws = t.deps.WorkspaceFn(ctx)
	}
	for i, argv := range cmd.Steps {
		out.step = i
		res, err := RunArgv(ctx, ws, argv, timeout)
		if err != nil {
			out.startErr = true
			out.exitCode = -1
			out.output = err.Error()
			break
		}
		out.exitCode = res.ExitCode
		out.timedOut = res.TimedOut
		out.cancelled = res.Cancelled
		out.output = strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
		if res.ExitCode != 0 || res.TimedOut || res.Cancelled {
			break
		}
	}
	out.elapsed = time.Since(start)
	return out
}

// announce tells the language server and caches that the file changed on disk.
// Best-effort in both directions (mutate and restore): a notification failure
// must never affect the verdict or the restore.
func (t *MutationTest) announce(ctx context.Context, path string) {
	_ = notifyLSP(ctx, t.deps.Client, path, protocol.FileChanged)
	invalidateCache(t.deps.Cache, path)
}

// restore rewrites the pre-mutation snapshot and PROVES it landed by comparing
// the file's SHA-256 to the snapshot's. It runs under context.WithoutCancel so
// a cancelled or expired request still restores — the one operation in this
// tool that must not be cancellable.
//
// A verified restore is silent. An unverified one is escalated: the snapshot is
// written to a sidecar and an error naming both the file and its recovery is
// returned, because leaving a mutated source file behind while reporting
// success is the worst thing this tool could do.
func (t *MutationTest) restore(ctx context.Context, tgt mutationTarget) error {
	ctx = context.WithoutCancel(ctx)
	if _, err := safeWrite(tgt.path, tgt.original, tgt.mode); err != nil {
		return t.restoreFailed(tgt, fmt.Sprintf("rewriting the original content failed: %v", err))
	}
	sha, err := fileSHA256(tgt.path)
	if err != nil {
		return t.restoreFailed(tgt, fmt.Sprintf("the restored file could not be re-hashed to verify it: %v", err))
	}
	if sha != tgt.sha {
		return t.restoreFailed(tgt, fmt.Sprintf("the restored content does not match the pre-run snapshot (sha256 %s, want %s)", sha, tgt.sha))
	}
	t.announce(ctx, tgt.path)
	t.deps.notifyTopology(tgt.path)
	return nil
}

// restoreFailed writes the emergency sidecar and builds the escalation error.
func (t *MutationTest) restoreFailed(tgt mutationTarget, cause string) error {
	sidecar := tgt.path + mutationRestoreSuffix
	saved := "the pre-mutation content could not be saved to a sidecar either"
	if err := os.WriteFile(sidecar, tgt.original, tgt.mode); err == nil {
		saved = "the pre-mutation content has been saved to " + sidecar
	}
	return fmt.Errorf("mutation_test: RESTORE FAILED for %s — the file is still MUTATED on disk and must be restored by hand.\n"+
		"  cause: %s\n"+
		"  recover with: git checkout -- %s   (safe: mutation_test refused to start unless the file was clean)\n"+
		"  %s",
		tgt.display, cause, tgt.display, saved)
}

// gitCleanliness reports whether path sits inside a git repository and, if so,
// whether it has uncommitted changes. Untracked counts as dirty: an untracked
// file has nothing at HEAD, so a failed restore would destroy it outright.
//
// Both answers come from ONE `git status` call: a non-zero exit means "not a
// repository" (or no git at all), which is the case the caller warns about —
// the two states must not be collapsed, because "clean" and "no safety net"
// look identical from the output alone.
func gitCleanliness(ctx context.Context, path string) (inRepo, dirty bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return false, false
	}
	base := filepath.Base(path)
	args := gitReadArgv([]string{"status", "--porcelain", "--", base})
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: argv is package literals plus one basename passed after the "--" separator, so it cannot be read as a flag
	cmd.Dir = filepath.Dir(path)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, false // not a repository
		}
		return false, false // git unusable — treat as no safety net
	}
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			return true, true
		}
	}
	return true, false
}
