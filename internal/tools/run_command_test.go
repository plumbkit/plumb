package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunCommand_ArgValidation(t *testing.T) {
	rc := NewRunCommand(func(name, target string) (ResolvedCommand, error) {
		return ResolvedCommand{}, nil
	})
	cases := []struct {
		name string
		args string
	}{
		{"missing name", `{}`},
		{"blank name", `{"name":"  "}`},
		{"bad target", `{"name":"x","target":"a b; rm"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := rc.Execute(context.Background(), json.RawMessage(tc.args)); err == nil {
				t.Errorf("Execute(%s) = nil error, want error", tc.args)
			}
		})
	}
}

func TestRunCommand_ResolverErrorSurfaces(t *testing.T) {
	rc := NewRunCommand(func(name, target string) (ResolvedCommand, error) {
		return ResolvedCommand{}, errUntrusted
	})
	_, err := rc.Execute(context.Background(), json.RawMessage(`{"name":"lint"}`))
	if err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("err = %v, want the resolver's trust error", err)
	}
}

func TestRunCommand_NilResolver(t *testing.T) {
	rc := NewRunCommand(nil)
	if _, err := rc.Execute(context.Background(), json.RawMessage(`{"name":"x"}`)); err == nil {
		t.Fatal("want error when no resolver is wired")
	}
}

func TestRunCommand_Runs(t *testing.T) {
	rc := NewRunCommand(func(name, target string) (ResolvedCommand, error) {
		return ResolvedCommand{
			Name:       name,
			Argv:       []string{"echo", "hello-" + target},
			WorkingDir: t.TempDir(),
			Provenance: "global",
		}, nil
	})
	out, err := rc.Execute(context.Background(), json.RawMessage(`{"name":"greet","target":"world"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "hello-world") {
		t.Errorf("output missing command result:\n%s", out)
	}
	if !strings.Contains(out, "→ ok") {
		t.Errorf("output missing ok marker:\n%s", out)
	}
	if !strings.Contains(out, "run_command greet") {
		t.Errorf("output missing header:\n%s", out)
	}
}

func TestRunCommand_ReportsNetworkOff(t *testing.T) {
	rc := NewRunCommand(func(name, target string) (ResolvedCommand, error) {
		return ResolvedCommand{
			Name:       name,
			Argv:       []string{"echo", "x"},
			WorkingDir: t.TempDir(),
			Provenance: "project",
			Sandbox:    SandboxOpts{WorkspaceRoot: t.TempDir(), DenyNetwork: true},
		}, nil
	})
	out, err := rc.Execute(context.Background(), json.RawMessage(`{"name":"x"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "network=off") {
		t.Errorf("expected network=off in the run_command reply:\n%s", out)
	}
}

var errUntrusted = &trustErr{}

type trustErr struct{}

func (*trustErr) Error() string {
	return `run_command: "lint" comes from this project's .plumb/config.toml and is not trusted. run ` + "`plumb trust`"
}
