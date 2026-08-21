package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func TestValidateCommands(t *testing.T) {
	cases := []struct {
		name    string
		cmds    []CommandConfig
		wantErr bool
	}{
		{"empty list", nil, false},
		{"valid", []CommandConfig{{Name: "lint", Exec: []string{"golangci-lint", "run"}}}, false},
		{"valid with target", []CommandConfig{{Name: "t", Exec: []string{"go", "test", "-run", TargetToken, "./..."}}}, false},
		{"valid dot workdir", []CommandConfig{{Name: "l", Exec: []string{"go", "build"}, WorkingDir: "."}}, false},
		{"valid subdir workdir", []CommandConfig{{Name: "l", Exec: []string{"go", "build"}, WorkingDir: "internal/x"}}, false},
		{"blank name", []CommandConfig{{Name: "  ", Exec: []string{"x"}}}, true},
		{"duplicate name", []CommandConfig{{Name: "a", Exec: []string{"x"}}, {Name: "a", Exec: []string{"y"}}}, true},
		{"empty exec", []CommandConfig{{Name: "a", Exec: nil}}, true},
		{"blank exec0", []CommandConfig{{Name: "a", Exec: []string{"  ", "b"}}}, true},
		{"two targets", []CommandConfig{{Name: "a", Exec: []string{"go", TargetToken, TargetToken}}}, true},
		{"negative timeout", []CommandConfig{{Name: "a", Exec: []string{"x"}, Timeout: Duration{-1}}}, true},
		{"absolute workdir", []CommandConfig{{Name: "a", Exec: []string{"x"}, WorkingDir: "/etc"}}, true},
		{"escaping workdir", []CommandConfig{{Name: "a", Exec: []string{"x"}, WorkingDir: "../../etc"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCommands(tc.cmds)
			if tc.wantErr && err == nil {
				t.Fatalf("validateCommands(%s) = nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateCommands(%s) = %v, want nil", tc.name, err)
			}
		})
	}
}

// TestValidateCommands_MetacharsAllowedInArgv guards the deliberate choice: an
// exec argv is run without a shell, so a metacharacter is a literal argument, not
// syntax. It must NOT be rejected (unlike a [tasks] command string).
func TestValidateCommands_MetacharsAllowedInArgv(t *testing.T) {
	cmds := []CommandConfig{{Name: "grep", Exec: []string{"sh", "-c", "go test ./... | grep PASS"}}}
	if err := validateCommands(cmds); err != nil {
		t.Fatalf("validateCommands rejected a literal-metachar argv: %v", err)
	}
}

func TestFindCommandAndNames(t *testing.T) {
	cmds := []CommandConfig{{Name: "a", Exec: []string{"x"}}, {Name: "b", Exec: []string{"y"}}}
	if c, ok := FindCommand(cmds, "b"); !ok || c.Exec[0] != "y" {
		t.Fatalf("FindCommand(b) = %+v, %v", c, ok)
	}
	if _, ok := FindCommand(cmds, "missing"); ok {
		t.Fatal("FindCommand(missing) reported found")
	}
	got := CommandNames(cmds)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("CommandNames = %v", got)
	}
}

func TestCloneCommands_DeepCopy(t *testing.T) {
	base := []CommandConfig{{Name: "a", Exec: []string{"go", "build"}}}
	cl := cloneCommands(base)
	cl[0].Exec[1] = "mutated"
	if base[0].Exec[1] == "mutated" {
		t.Fatal("cloneCommands did not deep-copy the Exec slice")
	}
	if cloneCommands(nil) != nil {
		t.Fatal("cloneCommands(nil) should return nil")
	}
}

// TestEmptyCommandsAreNotSerialised guards the interaction that made shipped
// defaults inert.
//
// agent_write marshals the WHOLE config back to disk. Without omitempty an empty
// allow-list is written as a literal `command = []`, and an explicit empty array
// in a user's file out-ranks the compiled-in default from then on. A real global
// config here carried exactly that line — written by plumb itself, never typed by
// anyone — so `run_command` answered "no commands are configured" while the
// binary shipped three.
//
// The general shape is worth keeping in mind beyond commands: a config writer
// that materialises every field freezes today's defaults into every user's file.
func TestEmptyCommandsAreNotSerialised(t *testing.T) {
	data, err := toml.Marshal(Config{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "command = []") {
		t.Errorf("an empty allow-list must not be written; it would override the shipped "+
			"defaults permanently. Got:\n%s", data)
	}
}

// TestDefaults_CommandsAreReadOnlyAndBounded asserts the bar any shipped
// [[command]] entry must clear, rather than asserting there are none.
//
// The earlier test pinned "defaults ship no commands" with no recorded reason,
// which is not a coherent posture on its own: [tasks] has always shipped
// `go test ./...` enabled by default, and that runs arbitrary code out of the
// repository. What matters is not that the list is empty but that anything in it
// is safe.
//
// Three entries were shipped against this bar and reverted, because the bar was
// incomplete: it checks writes, network and placeholder COUNT, and says nothing
// about the placeholder's VALUE — which is the property that mattered, since a
// {target} is only shell-safe, not workspace-confined. Anyone shipping defaults
// must extend this test to cover target confinement and an explicit timeout, not
// just satisfy what is here.
func TestDefaults_CommandsAreReadOnlyAndBounded(t *testing.T) {
	d := Defaults()
	// Currently none are shipped — config_commands.go records why they were
	// reverted and what must land first.
	// The loop stays so the bar is already enforced whenever entries return.
	for _, c := range d.Commands {
		if c.Name == "" || len(c.Exec) == 0 {
			t.Errorf("default command %+v must have a name and an exec argv", c)
		}
		if c.AllowWrites {
			t.Errorf("default command %q sets allow_writes; shipped defaults must be read-only", c.Name)
		}
		if !c.DenyNetwork {
			t.Errorf("default command %q permits the network; shipped defaults must deny it", c.Name)
		}
		// At most one placeholder, and only the bounded {target} forms — anything
		// else would mean free text reaching an argv nobody validated.
		placeholders := 0
		for _, arg := range c.Exec {
			if arg == TargetToken || strings.HasPrefix(arg, TargetTokenPrefix) {
				placeholders++
				continue
			}
			if strings.Contains(arg, "{") {
				t.Errorf("default command %q has an unrecognised placeholder in %q", c.Name, arg)
			}
		}
		if placeholders > 1 {
			t.Errorf("default command %q uses %d placeholders; at most one is allowed", c.Name, placeholders)
		}
	}
	if d.CommandPolicy.AllowShell {
		t.Fatal("Defaults must have allow_shell = false")
	}
	if !d.CommandPolicy.DenyNetwork {
		t.Fatal("Defaults must deny the shell tier's network by default (deny_network = true)")
	}
}

// TestSetProjectValue_CommandArrayRoundTrips guards the TUI's workspace-scope
// save: the [[command]] array is written as a whole via SetProjectValue, so it
// must serialise to valid array-of-tables TOML and read back intact.
func TestSetProjectValue_CommandArrayRoundTrips(t *testing.T) {
	ws := t.TempDir()
	cmds := []CommandConfig{
		{Name: "lint", Exec: []string{"golangci-lint", "run"}, WorkingDir: "internal", Timeout: Duration{90 * time.Second}, AllowWrites: true},
		{Name: "test-one", Exec: []string{"go", "test", "-run", "{target}", "./..."}, DenyNetwork: true},
	}
	if err := SetProjectValue(ws, []string{"command"}, cmds); err != nil {
		t.Fatalf("SetProjectValue: %v", err)
	}
	got, err := LoadProject(Defaults(), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if len(got.Commands) != 2 {
		t.Fatalf("round-trip lost commands: %+v", got.Commands)
	}
	lint, ok := FindCommand(got.Commands, "lint")
	if !ok || lint.WorkingDir != "internal" || lint.Timeout.Duration != 90*time.Second || !lint.AllowWrites {
		t.Fatalf("lint did not round-trip: %+v", lint)
	}
	if len(lint.Exec) != 2 || lint.Exec[0] != "golangci-lint" || lint.Exec[1] != "run" {
		t.Fatalf("lint exec did not round-trip: %v", lint.Exec)
	}
	one, ok := FindCommand(got.Commands, "test-one")
	if !ok || !one.DenyNetwork || len(one.Exec) != 5 || one.Exec[3] != "{target}" {
		t.Fatalf("test-one did not round-trip: %+v", one)
	}
}

func writeProjectConfig(t *testing.T, ws, body string) {
	t.Helper()
	dir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
}

func TestLoadProject_MergesCommands(t *testing.T) {
	ws := t.TempDir()
	writeProjectConfig(t, ws, `
[[command]]
name = "test-one"
exec = ["go", "test", "-run", "{target}", "./..."]
timeout = "90s"
allow_writes = true

[commands]
allow_shell = true
require_sandbox = true
deny_network = true
`)
	got, err := LoadProject(Defaults(), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	c, ok := FindCommand(got.Commands, "test-one")
	if !ok {
		t.Fatalf("merged commands missing test-one: %+v", got.Commands)
	}
	if c.Timeout.Duration != 90*time.Second || !c.AllowWrites {
		t.Fatalf("command fields not merged: %+v", c)
	}
	if !got.CommandPolicy.AllowShell || !got.CommandPolicy.RequireSandbox || !got.CommandPolicy.DenyNetwork {
		t.Fatalf("policy not merged: %+v", got.CommandPolicy)
	}
}

// TestLoadProject_ProjectCommandsShadowGlobal documents the array-replace merge:
// when a project declares its own [[command]] block, it REPLACES the global
// allow-list entirely (global entries are shadowed), rather than appending.
func TestLoadProject_ProjectCommandsShadowGlobal(t *testing.T) {
	base := Defaults()
	base.Commands = []CommandConfig{{Name: "global-only", Exec: []string{"echo", "g"}}}
	ws := t.TempDir()
	writeProjectConfig(t, ws, `
[[command]]
name = "project-only"
exec = ["echo", "p"]
`)
	got, err := LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if _, ok := FindCommand(got.Commands, "project-only"); !ok {
		t.Error("project command missing after merge")
	}
	if _, ok := FindCommand(got.Commands, "global-only"); ok {
		t.Error("global command must be shadowed when the project declares its own [[command]] block")
	}
}

func TestLoadProject_RejectsInvalidCommand(t *testing.T) {
	ws := t.TempDir()
	writeProjectConfig(t, ws, `
[[command]]
name = "dup"
exec = ["a"]
[[command]]
name = "dup"
exec = ["b"]
`)
	if _, err := LoadProject(Defaults(), ws); err == nil {
		t.Fatal("LoadProject accepted duplicate command names")
	}
}
