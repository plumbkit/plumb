package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// newProfileSession builds a struct-literal connSession with the given resolved
// [tools] config and client name, both read lock-free off the snapshot.
func newProfileSession(t *testing.T, tc config.ToolsConfig, client string) *connSession {
	t.Helper()
	s := &connSession{}
	s.mutate(func(v *sessionView) {
		v.tools = tc
		v.clientName = client
	})
	return s
}

func TestMaybeNotifyToolProfileChange_FiresOnChange(t *testing.T) {
	s := newProfileSession(t, config.ToolsConfig{Profile: "lean"}, "claude-code")
	var calls []string
	s.mutate(func(v *sessionView) {
		v.lastToolProfile = "lean"
		v.notify = func(method string, _ any) error {
			calls = append(calls, method)
			return nil
		}
		// Flip the resolved profile so the next call sees a change.
		v.tools = config.ToolsConfig{Profile: "full"}
	})

	s.maybeNotifyToolProfileChange()

	if len(calls) != 1 || calls[0] != "notifications/tools/list_changed" {
		t.Fatalf("want one tools/list_changed notification, got %v", calls)
	}
	if got := s.view().lastToolProfile; got != "full" {
		t.Errorf("lastToolProfile = %q, want %q after firing", got, "full")
	}
}

func TestMaybeNotifyToolProfileChange_NoFireWhenUnchanged(t *testing.T) {
	s := newProfileSession(t, config.ToolsConfig{Profile: "full"}, "claude-code")
	fired := false
	s.mutate(func(v *sessionView) {
		v.lastToolProfile = "full" // matches the resolved profile — no change
		v.notify = func(string, any) error {
			fired = true
			return nil
		}
	})

	s.maybeNotifyToolProfileChange()

	if fired {
		t.Error("no notification should fire when the resolved profile is unchanged")
	}
}

func TestMaybeNotifyToolProfileChange_NoNotifierIsNoOp(t *testing.T) {
	s := newProfileSession(t, config.ToolsConfig{Profile: "full"}, "claude-code")
	s.mutate(func(v *sessionView) { v.lastToolProfile = "lean" }) // a change, but notify is nil
	// Must not panic and must leave the seed untouched (nothing was advertised).
	s.maybeNotifyToolProfileChange()
	if got := s.view().lastToolProfile; got != "lean" {
		t.Errorf("lastToolProfile = %q, want %q (no-op when notifier is nil)", got, "lean")
	}
}

// TestResolveToolProfile_CodexAutoResolvesFull is a regression test for the
// incident where a Codex session was told "Tool profile: lean — 39 commodity
// tools hidden" while its host ALSO deferred plumb's tools from the model, so
// the model could not discover the hidden tools at all. Lean must be opt-in
// via an explicit, verified clientcaps.ReliableDeferredToolDiscovery
// declaration, never inferred from native file/search possession — codex has
// NativeFileRead/NativeSearch but no verified deferred-discovery capability,
// so auto mode must resolve it to full. The reason is now "client-side-allowlist"
// (PLAN-369): codex's ClientSideAllowlist flag earns it a distinct rung in the
// ladder from the generic conservative default, so the banner can render the
// client-side-filter caveat truthfully — the served profile is unchanged.
func TestResolveToolProfile_CodexAutoResolvesFull(t *testing.T) {
	s := newProfileSession(t, config.ToolsConfig{Profile: "auto"}, "codex")
	profile, reason := s.resolveToolProfile()
	if profile != "full" || reason != "client-side-allowlist" {
		t.Errorf("resolveToolProfile() = (%q, %q), want (\"full\", \"client-side-allowlist\")", profile, reason)
	}
}

// TestAutoProfileFor exercises the auto-mode policy directly against synthetic
// Capabilities, covering all four (profile, reason) outcomes without going
// through the clientcaps registry.
func TestAutoProfileFor(t *testing.T) {
	cases := []struct {
		name        string
		caps        clientcaps.Capabilities
		wantProfile string
		wantReason  string
	}{
		{
			"unrecognised client (new lean baseline, PLAN-369)",
			clientcaps.Capabilities{Name: "unknown"},
			"lean", "unknown-client-baseline",
		},
		{
			"schema-discovery-only client",
			clientcaps.Capabilities{Name: "some-client", SchemaDiscoveryOnly: true},
			"full", "schema-discovery-only-client",
		},
		{
			"verified deferred discovery",
			clientcaps.Capabilities{Name: "some-client", ReliableDeferredToolDiscovery: true},
			"lean", "verified-deferred-discovery",
		},
		{
			"client-side allowlist",
			clientcaps.Capabilities{Name: "some-client", ClientSideAllowlist: true},
			"full", "client-side-allowlist",
		},
		{
			"unverified deferred discovery (registered client, conservative default)",
			clientcaps.Capabilities{Name: "some-client"},
			"full", "unverified-deferred-discovery",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			profile, reason := autoProfileFor(c.caps)
			if profile != c.wantProfile || reason != c.wantReason {
				t.Errorf("autoProfileFor(%+v) = (%q, %q), want (%q, %q)", c.caps, profile, reason, c.wantProfile, c.wantReason)
			}
		})
	}
}

func TestResolveToolProfile(t *testing.T) {
	cases := []struct {
		name       string
		tc         config.ToolsConfig
		client     string
		want       string
		wantReason string
	}{
		{"auto + claude-code => full (schema-discovery only)", config.ToolsConfig{Profile: "auto"}, "claude-code", "full", "schema-discovery-only-client"},
		{"auto + codex => full (client-side allowlist)", config.ToolsConfig{Profile: "auto"}, "codex/1.2.3", "full", "client-side-allowlist"},
		{"auto + gemini => full (client-side allowlist)", config.ToolsConfig{Profile: "auto"}, "gemini-cli/1.0.0", "full", "client-side-allowlist"},
		{"auto + claude-desktop => full", config.ToolsConfig{Profile: "auto"}, "claude-ai", "full", "unverified-deferred-discovery"},
		{"auto + unrecognised client => lean baseline (PLAN-369)", config.ToolsConfig{Profile: "auto"}, "some-new-agent", "lean", "unknown-client-baseline"},
		{"explicit lean wins over desktop", config.ToolsConfig{Profile: "lean"}, "claude-ai", "lean", "explicit-config"},
		{"explicit full wins over claude-code", config.ToolsConfig{Profile: "full"}, "claude-code", "full", "explicit-config"},
		{"empty profile treated as auto", config.ToolsConfig{Profile: ""}, "codex", "full", "client-side-allowlist"},
		{
			"per-client override beats profile",
			config.ToolsConfig{Profile: "full", ClientProfiles: map[string]string{"claude-code": "lean"}},
			"claude-code", "lean", "client-override",
		},
		{
			"per-client auto falls through to profile",
			config.ToolsConfig{Profile: "full", ClientProfiles: map[string]string{"claude-code": "auto"}},
			"claude-code", "full", "explicit-config",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newProfileSession(t, c.tc, c.client)
			profile, reason := s.resolveToolProfile()
			if profile != c.want {
				t.Errorf("resolveToolProfile() profile = %q, want %q", profile, c.want)
			}
			if reason != c.wantReason {
				t.Errorf("resolveToolProfile() reason = %q, want %q", reason, c.wantReason)
			}
		})
	}
}

func TestToolVisible_LeanHidesCommodityKeepsLean(t *testing.T) {
	s := newProfileSession(t, config.ToolsConfig{Profile: "lean"}, "claude-code")
	if s.toolVisible("copy_file") {
		t.Error("lean profile should hide copy_file from tools/list")
	}
	if !s.toolVisible("read_file") {
		t.Error("lean profile must keep read_file (edit lane needs its headers)")
	}
	if !s.toolVisible("edit_file") {
		t.Error("lean profile must keep the mutation tool edit_file")
	}
	full := newProfileSession(t, config.ToolsConfig{Profile: "full"}, "claude-code")
	if !full.toolVisible("copy_file") {
		t.Error("full profile should advertise copy_file")
	}
}

// TestToolVisible_BootstrapAlwaysVisible is the independent-of-profile
// guarantee: every bootstrap tool must be visible under a lean-resolved
// connection, table-driven over all four names.
//
// The second half proves the guarantee does NOT merely piggyback on
// LeanTools membership (today bootstrap ⊂ lean, so a naive check here would
// pass even without the IsBootstrap fast path in toolVisible). It temporarily
// removes ALL FOUR bootstrap names from tools.LeanTools and re-checks each:
// without the fast path toolVisible falls through to tools.IsLean and would
// report false; with the fast path every one of them stays true regardless of
// LeanTools membership.
func TestToolVisible_BootstrapAlwaysVisible(t *testing.T) {
	s := newProfileSession(t, config.ToolsConfig{Profile: "lean"}, "claude-code")
	bootstrapNames := []string{"session_start", "git", "read_file", "edit_file"}
	for _, name := range bootstrapNames {
		if !s.toolVisible(name) {
			t.Errorf("bootstrap tool %q must be visible under the lean profile", name)
		}
	}

	saved := make(map[string]bool, len(bootstrapNames))
	for _, name := range bootstrapNames {
		saved[name] = tools.LeanTools[name]
		delete(tools.LeanTools, name)
	}
	defer func() {
		for name, v := range saved {
			tools.LeanTools[name] = v
		}
	}()
	for _, name := range bootstrapNames {
		if !s.toolVisible(name) {
			t.Errorf("%s must stay visible via the IsBootstrap fast path even when absent from LeanTools", name)
		}
	}
}

// leanConstructors maps the tools.New* constructor for each lean tool to its
// wire name. It mirrors tools.LeanTools (keyed by wire name) at the wiring layer
// so the guard below verifies the two representations agree and that every lean
// tool is still registered. A new tool defaults to the full profile (safe — full
// never hides anything), so only lean additions need an entry here.
var leanConstructors = map[string]string{
	"NewSessionStart":          "session_start",
	"NewReadFile":              "read_file",
	"NewReadSymbol":            "read_symbol",
	"NewFileOutline":           "file_outline",
	"NewEditFile":              "edit_file",
	"NewWriteFile":             "write_file",
	"NewRenameFile":            "rename_file",
	"NewDeleteFile":            "delete_file",
	"NewTransactionApply":      "transaction_apply",
	"NewUndoEdit":              "undo_edit",
	"NewGit":                   "git",
	"NewDiagnosticsWithOpener": "diagnostics",
	"NewGetDefinition":         "get_definition",
	"NewFindReferences":        "find_references",
	"NewRenameSymbol":          "rename_symbol",
	"NewWorkspaceSymbols":      "workspace_symbols",
	"NewTopologySearch":        "topology_search",
	"NewTopologyExplore":       "topology_explore",
	"NewTopologyAffected":      "topology_affected",
	"NewSearchMemories":        "search_memories",
	"NewTasks":                 "run_task",
}

// TestToolProfileClassification keeps the lean classification honest: the
// constructor→wire-name map agrees with tools.LeanTools, and every lean
// constructor is still wired into registerAllTools (so a rename or removal trips
// here instead of silently un-leaning a tool).
func TestToolProfileClassification(t *testing.T) {
	if len(leanConstructors) != len(tools.LeanTools) {
		t.Fatalf("leanConstructors has %d entries, tools.LeanTools has %d — keep them in lockstep",
			len(leanConstructors), len(tools.LeanTools))
	}
	for ctor, wire := range leanConstructors {
		if !tools.IsLean(wire) {
			t.Errorf("leanConstructors[%s]=%q is not in tools.LeanTools", ctor, wire)
		}
	}

	src, err := os.ReadFile("conn_register.go")
	if err != nil {
		t.Fatalf("reading conn_register.go: %v", err)
	}
	body := registerAllToolsBody(string(src))
	if body == "" {
		t.Fatal("could not locate registerAllTools in conn_register.go")
	}
	registered := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "srv.Register(tools.New") {
			continue
		}
		if name := extractToolName(trimmed); name != "" {
			registered[name] = true
		}
	}
	for ctor := range leanConstructors {
		if !registered[ctor] {
			t.Errorf("lean constructor %s is no longer registered in registerAllTools", ctor)
		}
	}
}

// TestAlwaysLoad_PinsTheMailboxPair asserts the real registerHooks wiring, not a
// restatement of it: the pin predicate the daemon installs must cover both
// halves of the mailbox, plus workspace_search (the documented discovery entry
// point that the old IsLean-derived wiring silently left deferred), while
// leaving the long tail deferred.
func TestAlwaysLoad_PinsTheMailboxPair(t *testing.T) {
	srv := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	(&connSession{}).registerHooks(srv)
	if srv.AlwaysLoad == nil {
		t.Fatal("registerHooks left AlwaysLoad unset — nothing is pinned at all")
	}
	for _, name := range []string{"leave_note", "check_messages", "workspace_search", "session_start"} {
		if !srv.AlwaysLoad(name) {
			t.Errorf("%q is not pinned; tools.PinnedTools must cover it", name)
		}
	}
	if srv.AlwaysLoad("topology_routes") {
		t.Error("topology_routes is pinned — the long tail must stay deferred, or the pin saves nothing")
	}
}

// TestAlwaysLoadDocMatchesWiring guards docs/configuration.md's "always-loaded
// (pinned) tools" paragraph against drift: whatever tools.Is* predicate
// srv.AlwaysLoad is wired to in conn_register.go, docs/configuration.md must
// name it, and must not still name a predicate the wiring dropped.
//
// The predicate list is read from the wiring itself, so the doc and the code
// must move together in BOTH directions: a predicate dropped from the code while
// the doc still names it fails just as loudly as one added without documenting.
func TestAlwaysLoadDocMatchesWiring(t *testing.T) {
	root := repoRootFromCaller(t)
	wired := alwaysLoadPredicates(t, root)
	if len(wired) == 0 {
		t.Fatal("no tools.Is* predicates found in the AlwaysLoad wiring — the extractor is broken")
	}

	b, err := os.ReadFile(filepath.Join(root, "docs/configuration.md"))
	if err != nil {
		t.Fatalf("reading docs/configuration.md: %v", err)
	}
	doc := string(b)
	for _, p := range wired {
		if !strings.Contains(doc, "tools."+p) {
			t.Errorf("docs/configuration.md does not name %q, which AlwaysLoad is wired to — "+
				"the pinned set it describes is wrong", "tools."+p)
		}
	}
	// The other direction: the doc must not advertise a predicate the code dropped.
	for _, p := range []string{"IsLean", "IsBootstrap", "IsMailbox", "IsPinned"} {
		if strings.Contains(doc, "tools."+p) && !slices.Contains(wired, p) {
			t.Errorf("docs/configuration.md names tools.%s but AlwaysLoad no longer uses it", p)
		}
	}
}

// alwaysLoadPredicates returns the tools.Is* predicate names appearing in the
// srv.AlwaysLoad assignment in conn_register.go. Matches both a direct
// function-value assignment (srv.AlwaysLoad = tools.IsPinned) and a wrapping
// closure that calls one or more tools.Is* predicates.
func alwaysLoadPredicates(t *testing.T, root string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "internal/cli/conn_register.go"))
	if err != nil {
		t.Fatalf("reading conn_register.go: %v", err)
	}
	src := string(b)
	start := strings.Index(src, "srv.AlwaysLoad = ")
	if start < 0 {
		t.Fatal("srv.AlwaysLoad assignment not found in conn_register.go")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end]
	}
	var out []string
	for _, m := range regexp.MustCompile(`tools\.(Is[A-Za-z]+)\b`).FindAllStringSubmatch(body, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}
