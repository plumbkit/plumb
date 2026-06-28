package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
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

func TestResolveToolProfile(t *testing.T) {
	cases := []struct {
		name   string
		tc     config.ToolsConfig
		client string
		want   string
	}{
		{"auto + claude-code => full (schema-discovery only)", config.ToolsConfig{Profile: "auto"}, "claude-code", "full"},
		{"auto + codex => lean", config.ToolsConfig{Profile: "auto"}, "codex/1.2.3", "lean"},
		{"auto + claude-desktop => full", config.ToolsConfig{Profile: "auto"}, "claude-ai", "full"},
		{"auto + unknown => full", config.ToolsConfig{Profile: "auto"}, "some-new-agent", "full"},
		{"explicit lean wins over desktop", config.ToolsConfig{Profile: "lean"}, "claude-ai", "lean"},
		{"explicit full wins over claude-code", config.ToolsConfig{Profile: "full"}, "claude-code", "full"},
		{"empty profile treated as auto", config.ToolsConfig{Profile: ""}, "codex", "lean"},
		{
			"per-client override beats profile",
			config.ToolsConfig{Profile: "full", ClientProfiles: map[string]string{"claude-code": "lean"}},
			"claude-code", "lean",
		},
		{
			"per-client auto falls through to profile",
			config.ToolsConfig{Profile: "full", ClientProfiles: map[string]string{"claude-code": "auto"}},
			"claude-code", "full",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newProfileSession(t, c.tc, c.client)
			if got := s.resolveToolProfile(); got != c.want {
				t.Errorf("resolveToolProfile() = %q, want %q", got, c.want)
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
