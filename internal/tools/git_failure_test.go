package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// TestGitWriteTimeoutError_BlamesPlumbNotGit is the unit half of the
// misattribution fix. The old report for this failure was `git commit: exit
// code -1` (a SIGKILLed child has no exit status) under a remediation asserting
// that git declined and no plumb setting changes the outcome — every clause of
// which points the reader at their own change. Each assertion below names one
// clause that must be true instead.
func TestGitWriteTimeoutError_BlamesPlumbNotGit(t *testing.T) {
	err := gitWriteTimeoutError("/r", "commit", []string{"commit", "-m", "x"}, 90*time.Second,
		"running pre-commit hook\n", "", "")

	msg := err.Error()
	for _, want := range []string{
		"plumb stopped waiting",   // plumb is named as the cause
		"1m30s",                   // the elapsed bound is named
		"killed the git child",    // what plumb did
		"running pre-commit hook", // the captured evidence survives
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message must contain %q, got %q", want, msg)
		}
	}
	if strings.Contains(msg, "exit code") {
		t.Errorf("a killed child has no meaningful exit code; reporting one is the bug: %q", msg)
	}

	// retry_after_wait, not inspect_output: there IS something the caller can do.
	assertClassified(t, err, toolerror.KindClientTimeout, toolerror.ClassRetryAfterWait, true)
	te := mustClassify(t, err)
	reason := te.Remediation.Reason
	for _, want := range []string{"write_timeout", "PLUMB_GIT_WRITE_TIMEOUT", "check the repository state"} {
		if !strings.Contains(reason, want) {
			t.Errorf("remediation must name %q, got %q", want, reason)
		}
	}
	if strings.Contains(reason, "no plumb setting or flag changes the outcome") {
		t.Errorf("the remediation still claims plumb is not involved: %q", reason)
	}
	if te.Details["write_timeout"] != "1m30s" {
		t.Errorf("Details[write_timeout] = %q, want the bound that expired", te.Details["write_timeout"])
	}
	if _, ok := te.Details["exit_code"]; ok {
		t.Errorf("Details must not carry an exit code for a killed child: %v", te.Details)
	}
}

// TestGitWriteTimeoutError_QuotesTruncatedOutputToo pins that the timeout
// report keeps the same bounded-quote machinery as an ordinary failure — the
// hook output is usually the only evidence of WHAT the bound was spent on.
func TestGitWriteTimeoutError_QuotesTruncatedOutputToo(t *testing.T) {
	err := gitWriteTimeoutError("/r", "commit", []string{"commit", "-m", "x"}, time.Minute,
		strings.Repeat("waiting\n", 200), "", "")
	if !strings.Contains(err.Error(), "re-run `git commit -m x` in /r") {
		t.Errorf("a truncated quote must still point at the full output, got %q", err.Error())
	}
	if got := mustClassify(t, err).Details["output_truncated"]; got != "true" {
		t.Errorf("Details[output_truncated] = %q, want \"true\"", got)
	}
}

// TestLintLockHint_OnlyOnTheLiteralMarker is the safety half of the hook-output
// check: it fires on golangci-lint's own words for lock contention and on
// nothing else. A false "this is just contention" on a real lint failure would
// be worse than the bug it addresses, so the trigger is a literal string one
// program emits for one condition, matched on either stream.
func TestLintLockHint_OnlyOnTheLiteralMarker(t *testing.T) {
	tests := []struct {
		name           string
		stdout, stderr string
		want           bool
	}{
		{"marker on stdout", "ERROR: parallel golangci-lint is running\n", "", true},
		{"marker on stderr", "", "ERROR: parallel golangci-lint is running\n", true},
		{"a real lint failure", "internal/tools/git.go:12:3: err unchecked (errcheck)\n", "", false},
		{"a hook merely mentioning the linter", "running golangci-lint...\n", "", false},
		{"nothing at all", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lintLockHint(tt.stdout, tt.stderr) != ""; got != tt.want {
				t.Errorf("lintLockHint fired = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGitCommandError_LintLockIsRetryableNotYourFault covers the one hook
// failure that is not about the repository at all. golangci-lint refuses to
// start while a concurrent run holds its shared cache lock, the hook exits
// non-zero, and the report said "git declined the operation … no plumb setting
// or flag changes the outcome" — which reads as a rejected change and is not
// retryable, when waiting and retrying is precisely the remedy.
func TestGitCommandError_LintLockIsRetryableNotYourFault(t *testing.T) {
	err := gitCommandError("/r", "commit", []string{"commit", "-m", "x"}, exitError(t),
		"ERROR: parallel golangci-lint is running\n", "", "")

	assertClassified(t, err, toolerror.KindGitCommandFailed, toolerror.ClassRetryAfterWait, true)
	if !strings.Contains(err.Error(), "not a finding about this change") {
		t.Errorf("the message must say the failure is contention, got %q", err.Error())
	}
	// The hint ADDS to the real output; it never replaces or suppresses it.
	if !strings.Contains(err.Error(), "parallel golangci-lint is running") {
		t.Errorf("the hook's own output must survive alongside the hint, got %q", err.Error())
	}
}

// TestGitCommandError_OrdinaryFailureKeepsInspectOutput is the other half of the
// pair: without the marker the honest classification is unchanged, so the hint
// cannot quietly relabel every failing hook as transient.
func TestGitCommandError_OrdinaryFailureKeepsInspectOutput(t *testing.T) {
	err := gitCommandError("/r", "commit", []string{"commit", "-m", "x"}, exitError(t),
		"internal/tools/git.go:12:3: err unchecked (errcheck)\n", "", "")
	assertClassified(t, err, toolerror.KindGitCommandFailed, toolerror.ClassInspectOutput, false)
}

// TestGitChildSpec_ZeroMeansTheDefaultBound pins that an unset write_timeout
// resolves to the compiled default rather than to "no bound". GitPolicy is
// built by hand in tests and by any consumer indifferent to this knob, and a
// zero there reaching context.WithTimeout would produce an already-expired
// context — killing every commit instantly — while treating zero as "unbounded"
// would let a wedged child hold the repository lock forever. Neither is
// reachable; both would be, one typo apart.
func TestGitChildSpec_ZeroMeansTheDefaultBound(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"unset", 0, defaultGitWriteTimeout},
		{"negative", -time.Second, defaultGitWriteTimeout},
		{"configured", 30 * time.Second, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (gitChildSpec{WriteTimeout: tt.in}).writeTimeout(); got != tt.want {
				t.Errorf("writeTimeout() = %s, want %s", got, tt.want)
			}
		})
	}
	if defaultGitWriteTimeout <= 2*time.Minute {
		t.Errorf("the default is %s; the 2m it replaced is the value this change exists to raise", defaultGitWriteTimeout)
	}
}

// TestGitChildSpecFor_CarriesBothHalvesOfTheGitBlock guards the adapter that
// turns a policy into a child spec. Dropping either half is silent: the config
// parses, the trust disclosure lists it, and the child runs with the wrong
// environment or the wrong bound.
func TestGitChildSpecFor_CarriesBothHalvesOfTheGitBlock(t *testing.T) {
	spec := gitChildSpecFor(GitPolicy{
		Env:          map[string]string{"GOWORK": "off"},
		WriteTimeout: 42 * time.Second,
	})
	if spec.WriteTimeout != 42*time.Second {
		t.Errorf("WriteTimeout = %s, want 42s", spec.WriteTimeout)
	}
	var found bool
	for _, e := range spec.Env {
		if e == "GOWORK=off" {
			found = true
		}
	}
	if !found {
		t.Errorf("the configured git child environment did not cross over: %v", spec.Env)
	}
}
