package cli

import (
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestCommandsFromProject mirrors TestTaskProvenance: a [[command]] array in the
// project config marks its entries project-sourced (so run_command's trust gate
// applies), while no project array means the commands are global.
//
// It now asks the SESSION VIEW rather than a standalone helper. The helper it
// replaced looked the raw key up by exact name, which missed `[[COMMAND]]` and
// skipped the trust gate entirely; provenance is resolved from the policy spec
// at config apply instead. The fold spellings are exercised here as well as in
// conn_commands_trust_test.go, because this is the test that describes what
// "project-sourced" MEANS.
func TestCommandsFromProject(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	projectCommands := func(t *testing.T, body string) bool {
		t.Helper()
		ws := t.TempDir()
		if body != "" {
			writeExecProject(t, ws, body)
		}
		return execTrustSession(t, ws).view().projectCommands
	}

	if projectCommands(t, "") {
		t.Error("a workspace with no project config must report fromProject=false")
	}
	if projectCommands(t, "[edits]\nrate_limit_per_minute = 7\n") {
		t.Error("a project config with no [[command]] must report fromProject=false")
	}
	for _, body := range []string{
		"[[command]]\nname = \"lint\"\nexec = [\"golangci-lint\", \"run\"]\n",
		"[[COMMAND]]\nname = \"lint\"\nexec = [\"golangci-lint\", \"run\"]\n",
		"[[Command]]\nname = \"lint\"\nexec = [\"golangci-lint\", \"run\"]\n",
		"command = [{name = \"lint\", exec = [\"golangci-lint\", \"run\"]}]\n",
	} {
		if !projectCommands(t, body) {
			t.Errorf("a project config defining commands must report fromProject=true: %q", body)
		}
	}
}

// TestGatedAllowShell pins the trust rule for execute_shell_command: an untrusted
// project can neither enable shell (base wins) nor disable it; a trusted project's
// value is honoured in both directions.
func TestGatedAllowShell(t *testing.T) {
	on := config.CommandsConfig{AllowShell: true}
	off := config.CommandsConfig{AllowShell: false}
	cases := []struct {
		name    string
		base    config.CommandsConfig
		merged  config.CommandsConfig
		trusted bool
		want    bool
	}{
		{"untrusted project raise ignored", off, on, false, false},
		{"untrusted project lower ignored (base wins)", on, off, false, true},
		{"trusted project raise honoured", off, on, true, true},
		{"trusted project lower honoured", on, off, true, false},
		{"global on, no project change, untrusted", on, on, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatedAllowShell(tc.base, tc.merged, tc.trusted); got != tc.want {
				t.Errorf("gatedAllowShell(%+v, %+v, trusted=%v) = %v, want %v", tc.base, tc.merged, tc.trusted, got, tc.want)
			}
		})
	}
}

// TestGatedDenyNetwork mirrors the allow_shell gate for the shell tier's
// deny_network (default true): an untrusted project cannot re-open the network,
// a trusted one can.
func TestGatedDenyNetwork(t *testing.T) {
	on := config.CommandsConfig{DenyNetwork: true}
	off := config.CommandsConfig{DenyNetwork: false}
	cases := []struct {
		name    string
		base    config.CommandsConfig
		merged  config.CommandsConfig
		trusted bool
		want    bool
	}{
		{"default on, untrusted project re-open ignored", on, off, false, true},
		{"default on, trusted project re-open honoured", on, off, true, false},
		{"global off, untrusted project deny ignored", off, on, false, false},
		{"global off, trusted project deny honoured", off, on, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatedDenyNetwork(tc.base, tc.merged, tc.trusted); got != tc.want {
				t.Errorf("gatedDenyNetwork(%+v, %+v, trusted=%v) = %v, want %v", tc.base, tc.merged, tc.trusted, got, tc.want)
			}
		})
	}
}

func TestCommandWorkdir(t *testing.T) {
	ws := "/ws"
	if got := commandWorkdir(ws, ""); got != ws {
		t.Errorf("empty workdir = %q, want %q", got, ws)
	}
	if got := commandWorkdir(ws, "."); got != ws {
		t.Errorf("dot workdir = %q, want %q", got, ws)
	}
	if got := commandWorkdir(ws, "internal/x"); got != "/ws/internal/x" {
		t.Errorf("subdir workdir = %q", got)
	}
}
