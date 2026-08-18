package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestProjectPolicyTrustResult covers the decision half of the doctor check,
// which is where the user is told that a project config they wrote is being
// ignored. It is pure, so it does not depend on the host's real trust store.
func TestProjectPolicyTrustResult(t *testing.T) {
	spec := config.ProjectPolicySpec{
		{Key: "git.allow_push", Value: true},
		{Key: "lsp.html.root_markers", Value: []any{"index.html"}},
	}

	untrusted := projectPolicyTrustResult("/w", config.ProjectPolicyStatus{Spec: spec})
	if !untrusted.ok {
		t.Error("an untrusted project config is the SAFE state, not a doctor failure")
	}
	if !untrusted.warn {
		t.Error("an untrusted project config must warn — silence is the defect this check exists for")
	}
	for _, want := range []string{"NOT in effect", "git.allow_push", "lsp.html.root_markers"} {
		if !strings.Contains(untrusted.detail, want) {
			t.Errorf("detail %q missing %q", untrusted.detail, want)
		}
	}
	if !strings.Contains(untrusted.fix, "plumb trust") {
		t.Errorf("fix %q must name `plumb trust`", untrusted.fix)
	}

	trusted := projectPolicyTrustResult("/w", config.ProjectPolicyStatus{Spec: spec, Trusted: true})
	if !trusted.ok || trusted.warn {
		t.Error("a trusted project config is a clean pass")
	}
	if !strings.Contains(trusted.detail, "in effect") {
		t.Errorf("detail %q should say the keys are in effect", trusted.detail)
	}
}

// TestCheckProjectPolicyTrust_OmittedWhenNothingAsked verifies the row is absent
// for the overwhelmingly common project that sets no capability-granting key —
// a pass row with nothing to report is noise, and doctor has enough.
func TestCheckProjectPolicyTrust_OmittedWhenNothingAsked(t *testing.T) {
	ws := t.TempDir()
	if err := config.SetProjectValue(ws, []string{"edits", "strict"}, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := checkProjectPolicyTrust(ws); ok {
		t.Error("a project config with no capability-granting key must not produce a row")
	}
}

// TestConfirmTrust_RefusesWithoutTTYOrYes pins that a grant covering the argv of
// a process spawned as the user cannot be acquired by side effect. `plumb trust`
// used to print the disclosure and grant unconditionally, so
// `plumb trust > /dev/null` granted in silence and the "read it before answering
// for it" instruction had nothing to answer.
//
// The decision halves are asserted directly: whether the test binary's own stdin
// happens to be a terminal is an accident of how the suite was invoked, and must
// not decide whether this is tested.
func TestConfirmTrust_RefusesWithoutTTYOrYes(t *testing.T) {
	t.Cleanup(func() { trustAssumeYes = false })

	if err := nonInteractiveTrustError("/w"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("a non-interactive stdin must be refused with the --yes escape named, got %v", err)
	}

	trustAssumeYes = true
	if err := confirmTrust("/w"); err != nil {
		t.Errorf("--yes must grant non-interactively, got %v", err)
	}
}

// TestTrustAnswerDecision_OnlyExplicitYesGrants pins the prompt's default. Every
// answer that is not an explicit yes refuses — including an empty line and the
// empty string a closed or unreadable stdin yields, since a grant obtainable by
// an absent answer is the silent grant the prompt exists to replace.
func TestTrustAnswerDecision_OnlyExplicitYesGrants(t *testing.T) {
	for _, yes := range []string{"y\n", "Y\n", "yes\n", " YES \n", "y"} {
		if err := trustAnswerDecision(yes); err != nil {
			t.Errorf("%q should grant, got %v", yes, err)
		}
	}
	for _, no := range []string{"", "\n", "n\n", "no\n", "maybe\n", "ye\n", "1\n", "  \n"} {
		err := trustAnswerDecision(no)
		if err == nil {
			t.Errorf("%q must NOT grant", no)
			continue
		}
		// A closed or Ctrl-D'd read at a real prompt yields an empty answer; the
		// advice must still be there. (Redirected stdin no longer reaches this
		// path — term.IsTerminal refuses /dev/null at the gate.)
		if strings.TrimSpace(no) == "" && !strings.Contains(err.Error(), "--yes") {
			t.Errorf("an empty answer should name the --yes escape, got %q", err)
		}
	}
}

// TestPrintPolicyWarnings_SurvivesAPaddedKeySet pins the flooding defence. The
// key set is attacker-chosen — [git] is taken whole, deliberately — so a
// repository can pad it to push the dangerous line out of the scrollback. The
// per-key listing is capped and the warnings are printed after it, so whatever
// the repository writes, the warnings are the last thing above the prompt.
func TestPrintPolicyWarnings_SurvivesAPaddedKeySet(t *testing.T) {
	spec := make(config.ProjectPolicySpec, 0, 501)
	spec = append(spec, config.PolicyEntry{Key: "lsp.go.command", Value: "/bin/sh"})
	for i := range 500 {
		spec = append(spec, config.PolicyEntry{Key: fmt.Sprintf("git.pad%03d", i), Value: true})
	}
	out := captureStdout(t, func() { printTrustedPolicy("/w", spec, config.Defaults()) })

	lines := strings.Count(out, "\n")
	if lines > policyDisclosureLimit+20 {
		t.Errorf("disclosure ran to %d lines for a padded key set; the listing must be capped", lines)
	}
	if !strings.Contains(out, "/bin/sh") {
		t.Error("the padded set scrolled the dangerous value out of the disclosure")
	}
	warnIdx := strings.Index(out, "grant capability")
	if warnIdx < 0 {
		t.Fatal("the warnings block is missing")
	}
	if strings.Index(out, "git.pad") > warnIdx {
		t.Error("padding printed after the warnings; warnings must come last")
	}
}

// TestPolicySourceFor_AnnotatesIgnoredRequest pins the `config show` provenance
// column. "global config" is a true answer to where the effective value came
// from and a misleading answer to what the project asked for; the annotation is
// what keeps the two apart.
func TestPolicySourceFor_AnnotatesIgnoredRequest(t *testing.T) {
	st := config.ProjectPolicyStatus{Spec: config.ProjectPolicySpec{{Key: "lsp.go.command", Value: "/bin/sh"}}}

	if got := policySourceFor(st, "lsp.go.command", "global config"); !strings.Contains(got, "UNTRUSTED") {
		t.Errorf("source = %q, want the untrusted-request annotation", got)
	}
	if got := policySourceFor(st, "git.allow_push", "default"); got != "default" {
		t.Errorf("a key the project did not set must keep its plain source, got %q", got)
	}

	st.Trusted = true
	if got := policySourceFor(st, "lsp.go.command", "global config"); got != "project config (trusted)" {
		t.Errorf("a trusted request should be attributed to the project, got %q", got)
	}
}
