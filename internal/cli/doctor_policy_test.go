package cli

import (
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
