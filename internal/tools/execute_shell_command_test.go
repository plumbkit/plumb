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
