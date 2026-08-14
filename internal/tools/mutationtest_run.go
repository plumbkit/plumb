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
type stepOutcome struct {
	ran      bool
	exitCode int
	timedOut bool
	elapsed  time.Duration
	output   string
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

	res.classify(t.runStep(ctx, plan.compile, plan.timeout), t.runStep(ctx, plan.test, plan.timeout))
	return res, nil
}

// classify turns the two step outcomes into a verdict. It is the whole point of
// the tool, so it is one small readable function with no side effects.
//
// A test failure is a kill ONLY when the compile gate passed. Every other shape
// — a mutant that did not compile, a compile that timed out, a test run that
// timed out — is invalid, because none of them distinguishes "the assertion
// caught the change" from "the toolchain never got far enough to try".
func (r *mutationResult) classify(compile, test stepOutcome) {
	r.compile = compile
	switch {
	case compile.timedOut:
		r.outcome, r.reason = MutationInvalid, reasonCompileTimeout
		return
	case compile.exitCode != 0:
		r.outcome, r.reason = MutationInvalid, reasonCompileFailed
		return
	}
	r.test = test
	switch {
	case test.timedOut:
		r.outcome, r.reason = MutationInvalid, reasonTestTimeout
	case test.exitCode != 0:
		r.outcome = MutationKilled
	default:
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
// not on PATH) is reported as a non-zero step rather than aborting the run, so
// it lands in the invalid bucket instead of masquerading as a kill.
func (t *MutationTest) runStep(ctx context.Context, cmd TaskCommand, timeout time.Duration) stepOutcome {
	var out stepOutcome
	out.ran = true
	start := time.Now()
	ws := ""
	if t.deps.WorkspaceFn != nil {
		ws = t.deps.WorkspaceFn()
	}
	for _, argv := range cmd.Steps {
		res, err := RunArgv(ctx, ws, argv, timeout)
		if err != nil {
			out.exitCode = -1
			out.output = err.Error()
			break
		}
		out.exitCode = res.ExitCode
		out.timedOut = res.TimedOut
		out.output = strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
		if res.ExitCode != 0 || res.TimedOut {
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
