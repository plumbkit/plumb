package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestDefaults_NoCommands(t *testing.T) {
	d := Defaults()
	if d.Commands != nil {
		t.Fatalf("Defaults ship no allow-list commands; got %v", d.Commands)
	}
	if d.CommandPolicy.AllowShell {
		t.Fatal("Defaults must have allow_shell = false")
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
