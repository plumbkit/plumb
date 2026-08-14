package tools

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// git_failure.go holds how a failed git operation is REPORTED, as opposed to
// git_exec.go's how it is run. It is its own file because the report is where a
// failure is most easily misattributed: the same captured streams back both
// "git declined this" and "plumb stopped waiting for it", and those two have
// opposite remedies — one is fixed in the repository, the other by a plumb
// setting or by retrying. Getting the attribution wrong sends the caller to
// look for a defect in a change that has none.

const (
	// maxGitErrStreamBytes bounds each stream quoted in a failure response.
	maxGitErrStreamBytes = 16 * 1024
	// maxGitErrStdoutLines bounds the trailing stdout lines quoted on failure.
	maxGitErrStdoutLines = 40
)

// gitFailureBody renders the shared shape of every git failure report: the
// repo-intent advisory, then the headline, then stderr and stdout each quoted
// under their own label and bounded. The second return reports whether either
// quote was cut.
//
// warning (may be "") is the repo-intent advisory block computed before the git
// child ran (git_intent_warn.go) — it leads the message on a failure just as it
// leads the response on success, so a peer's live claim explaining the failure
// (e.g. a rebase collision) is not silently dropped by the one path where it
// matters most. Quoting both streams under labels, rather than letting whichever
// one is non-empty become the error string, is what stopped a failing pre-commit
// hook that wrote only to stdout surfacing as `git commit: 0 issues.
// file-size: OK` — the hook's chatter standing in for the real cause.
func gitFailureBody(repoRoot, headline, stdout, stderr, warning string) (string, bool) {
	var b strings.Builder
	b.WriteString(warning)
	b.WriteString(headline)
	truncated := false
	if msg, cut := boundGitErrStream(stderr); msg != "" {
		// The hint rewrites (index.lock, pathspec, submodule) all match git's
		// own diagnostics, which git writes to stderr — enhancing this stream
		// preserves their behaviour exactly.
		fmt.Fprintf(&b, "\nstderr:\n%s", enhanceGitError(repoRoot, msg))
		truncated = truncated || cut
	}
	if out, cut := tailGitErrStdout(stdout); out != "" {
		fmt.Fprintf(&b, "\nstdout (last %d lines):\n%s", maxGitErrStdoutLines, out)
		truncated = truncated || cut
	}
	return b.String(), truncated
}

// gitCommandError builds the tool-facing error for a git child that RAN and
// exited non-zero — git's own refusal, where the captured output holds the
// answer. A child plumb killed at its own deadline is not this case and must
// not be reported through here: see gitWriteTimeoutError.
//
// Success-path output is unaffected; this runs only on a failed run.
func gitCommandError(repoRoot, sub string, argv []string, runErr error, stdout, stderr, warning string) error {
	headline := fmt.Sprintf("git %s: %s", sub, gitExitDescription(runErr))
	body, truncated := gitFailureBody(repoRoot, headline, stdout, stderr, warning)
	if hint := lintLockHint(stdout, stderr); hint != "" {
		body += hint
	}
	if truncated {
		body += "\n" + gitRerunNote(argv, repoRoot)
	}
	return toolerror.New(toolerror.KindGitCommandFailed, errors.New(body),
		gitCommandRemediation(stdout, stderr),
		gitFailureDetails(sub, runErr, truncated)...)
}

// gitWriteTimeoutError reports an index/ref-mutating git child that PLUMB ended
// at its own bound, which is a different failure from the one above and used to
// be indistinguishable from it. A killed child yields ExitCode() == -1, so it
// rendered as `git commit: exit code -1` under a remediation asserting that
// "plumb raised no objection, so no plumb setting or flag changes the outcome"
// — both halves false, and in the direction that sends the caller hunting for a
// defect in their own change. The headline names plumb as the cause and the
// bound as the reason, and the remediation names the setting that moves it.
//
// The captured streams are still quoted: a hook that was mid-run when the bound
// expired has usually already printed what it was waiting on, and that is the
// evidence for deciding whether to raise the bound or fix the hook.
func gitWriteTimeoutError(repoRoot, sub string, argv []string, bound time.Duration, stdout, stderr, warning string) error {
	headline := fmt.Sprintf("git %s: plumb stopped waiting after %s and killed the git child", sub, bound)
	body, truncated := gitFailureBody(repoRoot, headline, stdout, stderr, warning)
	if hint := lintLockHint(stdout, stderr); hint != "" {
		body += hint
	}
	if truncated {
		body += "\n" + gitRerunNote(argv, repoRoot)
	}
	// KindClientTimeout, not KindGitCommandFailed: that kind's whole contract is
	// "plumb permitted the operation; git itself declined, and the remedy lies
	// entirely in the repository", which is exactly what did NOT happen here.
	// KindClientTimeout is plumb abandoning a call at its own deadline, which is
	// what this is.
	return toolerror.New(toolerror.KindClientTimeout, errors.New(body),
		toolerror.Remediation{
			Class: toolerror.ClassRetryAfterWait,
			Reason: fmt.Sprintf(
				"plumb ended this operation, not git: an index/ref-mutating git child is bounded at %s "+
					"([git] write_timeout, env PLUMB_GIT_WRITE_TIMEOUT) and that bound expired, so the child's process group was killed. "+
					"git may have finished some or all of the work before the kill — check the repository state (status, log -1) before retrying. "+
					"Raise write_timeout if this repository's hooks legitimately need longer; a pre-commit hook queued behind another agent's golangci-lint routinely does.",
				bound),
		},
		toolerror.WithDetail("subcommand", sub),
		toolerror.WithDetail("write_timeout", bound.String()),
		toolerror.WithDetail("output_truncated", strconv.FormatBool(truncated)),
	)
}

// lintLockMarker is golangci-lint's own words for "another run holds the shared
// cache lock". Matching its literal text is the whole reason this check is safe
// to make against free-form hook output: the string is emitted by one program,
// for one condition, and nothing about a repository's own state produces it.
const lintLockMarker = "parallel golangci-lint is running"

// lintLockHint annotates a hook failure that is not a finding about the change
// at all. A pre-commit hook running golangci-lint fails outright when a
// concurrent run holds the shared cache lock, and the hook's exit code is
// indistinguishable from a real lint failure — so plumb reported "git declined
// the operation; the captured output names the cause", and the output named a
// lint tool refusing to start. On a machine running several agents that is a
// routine event, and reading it as a rejected change costs a diagnosis every
// time.
//
// This ADDS a line beside the real output and suppresses nothing, so a genuine
// failure in the same run is still reported in full. Returns "" when the marker
// is absent.
func lintLockHint(stdout, stderr string) string {
	if !strings.Contains(stdout, lintLockMarker) && !strings.Contains(stderr, lintLockMarker) {
		return ""
	}
	return "\n  note: golangci-lint reported that a concurrent run holds its shared cache lock, so the hook failed " +
		"WITHOUT completing a lint pass. That is contention between runs on this machine, not a finding about this " +
		"change — wait for the peer run to finish and retry the same command."
}

// gitCommandRemediation decides what to tell the caller about a git child that
// exited non-zero. The default is the honest one: git declined and the captured
// output names why. The exception is the lock contention above, where the
// output names a cause that has nothing to do with the repository and where
// "no plumb setting or flag changes the outcome" would send the caller looking
// for a defect instead of simply retrying.
func gitCommandRemediation(stdout, stderr string) toolerror.Remediation {
	if lintLockHint(stdout, stderr) != "" {
		return toolerror.Remediation{
			Class: toolerror.ClassRetryAfterWait,
			Reason: "the hook failed on golangci-lint's shared cache lock, held by a concurrent run — " +
				"a transient condition on this machine, not something in the repository or in plumb's configuration. Retry once it clears.",
		}
	}
	return toolerror.Remediation{
		Class: toolerror.ClassInspectOutput,
		Reason: "git itself declined the operation; the captured stderr/stdout above names the cause. " +
			"plumb raised no objection, so no plumb setting or flag changes the outcome.",
	}
}

// gitFailureDetails carries the machine-readable half of a git failure: the
// exit code, the subcommand, and whether the quoted streams were cut. All three
// are low-cardinality by construction — the streams themselves stay in the
// message, where a bounded quote is safe, and never in Details.
func gitFailureDetails(sub string, runErr error, truncated bool) []toolerror.Option {
	opts := []toolerror.Option{
		toolerror.WithDetail("subcommand", sub),
		toolerror.WithDetail("output_truncated", strconv.FormatBool(truncated)),
	}
	if code, ok := gitExitCode(runErr); ok {
		opts = append(opts, toolerror.WithDetail("exit_code", strconv.Itoa(code)))
	}
	return opts
}

// gitExitCode reports the git child's exit code when it ran to completion, and
// ok=false when there was none (a start failure or a cancellation).
func gitExitCode(err error) (int, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

// gitExitDescription describes how the git child ended: its exit code when it
// ran to a non-zero exit, or the raw error when there is no exit code (a
// start failure or context cancellation).
func gitExitDescription(err error) string {
	if code, ok := gitExitCode(err); ok {
		return fmt.Sprintf("exit code %d", code)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return gitWaitDelayNote()
	}
	return err.Error()
}

// boundGitErrStream trims a captured stream and caps it at
// maxGitErrStreamBytes, keeping the tail — the most recent output, where the
// diagnosis lives. The second return reports whether truncation cut anything.
func boundGitErrStream(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) <= maxGitErrStreamBytes {
		return s, false
	}
	s = s[len(s)-maxGitErrStreamBytes:]
	// Drop the partial first line so the quote starts on a line boundary.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s, true
}

// tailGitErrStdout keeps the trailing lines of stdout for a failure report,
// capped at maxGitErrStdoutLines lines on top of the shared byte cap.
func tailGitErrStdout(s string) (string, bool) {
	s, truncated := boundGitErrStream(s)
	lines := strings.Split(s, "\n")
	if len(lines) > maxGitErrStdoutLines {
		lines = lines[len(lines)-maxGitErrStdoutLines:]
		truncated = true
	}
	return strings.Join(lines, "\n"), truncated
}

// gitRerunNote points at the way to see the complete output of a failed
// command whose quoted streams were truncated: run the same git invocation
// directly in the repository.
func gitRerunNote(argv []string, repoRoot string) string {
	return fmt.Sprintf("… (output truncated — re-run `git %s` in %s for the complete output)",
		quoteGitArgv(argv), repoRoot)
}

// quoteGitArgv renders an argv as a copy-pasteable command line, quoting any
// argument that contains whitespace or a single quote.
func quoteGitArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsAny(a, " \t'") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		quoted[i] = a
	}
	return strings.Join(quoted, " ")
}
