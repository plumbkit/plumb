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
	if untrusted.name != "capability trust" {
		t.Errorf("name = %q, want %q", untrusted.name, "capability trust")
	}
	// The detail is a head line plus one key per line: printCheck indents
	// continuation lines, so the key list stays readable however long it runs.
	lines := strings.Split(untrusted.detail, "\n")
	if lines[0] != "NOT in effect — this project's config sets 2 capability-granting key(s) plumb is ignoring:" {
		t.Errorf("first line = %q, want the ignoring head line", lines[0])
	}
	if last := lines[len(lines)-1]; last != "the global config's values are in force instead" {
		t.Errorf("last line = %q, want the global-values note", last)
	}
	keyLines := lines[1 : len(lines)-1]
	if len(keyLines) != 2 || keyLines[0] != "git.allow_push" || keyLines[1] != "lsp.html.root_markers" {
		t.Errorf("keys must be one per line, got %v", keyLines)
	}
	if !strings.Contains(untrusted.fix, "plumb trust") {
		t.Errorf("fix %q must name `plumb trust`", untrusted.fix)
	}

	trusted := projectPolicyTrustResult("/w", config.ProjectPolicyStatus{Spec: spec, Trusted: true})
	if !trusted.ok || trusted.warn {
		t.Error("a trusted project config is a clean pass")
	}
	if want := "trusted — 2 key(s) in effect\ngit.allow_push\nlsp.html.root_markers"; trusted.detail != want {
		t.Errorf("detail = %q, want %q", trusted.detail, want)
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

	// Under the test binary stdin is not a terminal, so the unadorned call is
	// exactly the refusal path a redirected `plumb trust` takes.
	if err := confirmTrust("/w"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("confirmTrust without a terminal or --yes must refuse, got %v", err)
	}

	trustAssumeYes = true
	if err := confirmTrust("/w"); err != nil {
		t.Errorf("--yes must grant non-interactively, got %v", err)
	}
}

// TestTrustConfirmation_OnlyExplicitYesGrants pins the selector's default. The
// cursor starts on No, so only the y key — or a deliberate move to Yes followed
// by enter — grants; everything else, including enter on the default cursor,
// refuses. A grant obtainable without a deliberate yes is the silent grant the
// prompt exists to replace.
func TestTrustConfirmation_OnlyExplicitYesGrants(t *testing.T) {
	newConfirm := func() yesNoModel {
		return yesNoModel{cursor: 1, render: renderTrustConfirmation}
	}
	grant := func(key string) bool {
		final, _ := newConfirm().Update(keyPress(key))
		return final.(yesNoModel).confirmed
	}
	for _, yes := range []string{"y", "Y"} {
		if !grant(yes) {
			t.Errorf("%q should grant", yes)
		}
	}
	for _, no := range []string{"n", "N", "q", "enter"} {
		if grant(no) {
			t.Errorf("%q must NOT grant (enter lands on the default No cursor)", no)
		}
	}

	// Moving to Yes and pressing enter grants; the move alone does not.
	moved, _ := newConfirm().Update(keyPress("up"))
	if m := moved.(yesNoModel); m.confirmed || m.cursor != 0 {
		t.Errorf("moving to Yes = (confirmed %v, cursor %d); the move must not grant by itself", m.confirmed, m.cursor)
	}
	final, _ := moved.Update(keyPress("enter"))
	if !final.(yesNoModel).confirmed {
		t.Error("enter on the Yes cursor must grant")
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
