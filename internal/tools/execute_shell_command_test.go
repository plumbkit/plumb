package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestExecuteShellCommand_EmptyCommand(t *testing.T) {
	sh := NewExecuteShellCommand(func() (ResolvedShell, error) { return ResolvedShell{}, nil })
	if _, err := sh.Execute(context.Background(), json.RawMessage(`{"command":"   "}`)); err == nil {
		t.Fatal("want error for a blank command")
	}
}

func TestExecuteShellCommand_DisabledSurfaces(t *testing.T) {
	sh := NewExecuteShellCommand(func() (ResolvedShell, error) {
		return ResolvedShell{}, fmt.Errorf("execute_shell_command is disabled. enable it with [commands] allow_shell = true")
	})
	_, err := sh.Execute(context.Background(), json.RawMessage(`{"command":"echo hi"}`))
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v, want the disabled gate error", err)
	}
}

func TestExecuteShellCommand_NilResolver(t *testing.T) {
	sh := NewExecuteShellCommand(nil)
	if _, err := sh.Execute(context.Background(), json.RawMessage(`{"command":"echo hi"}`)); err == nil {
		t.Fatal("want error when no resolver is wired")
	}
}

// TestExecuteShellCommand_ReportsNetworkStatus checks the reply tells the agent
// whether the network is off (with a hint it can relay) or on, so the agent can
// instruct the user to flip deny_network when a command needs the network.
func TestExecuteShellCommand_ReportsNetworkStatus(t *testing.T) {
	denied := NewExecuteShellCommand(func() (ResolvedShell, error) {
		return ResolvedShell{WorkingDir: t.TempDir(), Sandbox: SandboxOpts{WorkspaceRoot: t.TempDir(), DenyNetwork: true}}, nil
	})
	out, err := denied.Execute(context.Background(), json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "network=off") || !strings.Contains(out, "deny_network") {
		t.Errorf("expected a network-off notice the agent can relay:\n%s", out)
	}

	allowed := NewExecuteShellCommand(func() (ResolvedShell, error) {
		return ResolvedShell{WorkingDir: t.TempDir()}, nil
	})
	out2, err := allowed.Execute(context.Background(), json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out2, "network=on") {
		t.Errorf("expected network=on:\n%s", out2)
	}
}

func TestExecuteShellCommand_RunsWithPipe(t *testing.T) {
	sh := NewExecuteShellCommand(func() (ResolvedShell, error) {
		return ResolvedShell{WorkingDir: t.TempDir()}, nil
	})
	// A pipe proves sh -c interpretation (not a bare argv exec).
	out, err := sh.Execute(context.Background(), json.RawMessage(`{"command":"echo one two three | wc -w"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "3") {
		t.Errorf("expected word count 3 in output:\n%s", out)
	}
	if !strings.Contains(out, "→ ok") {
		t.Errorf("output missing ok marker:\n%s", out)
	}
}
