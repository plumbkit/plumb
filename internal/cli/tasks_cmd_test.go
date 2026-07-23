package cli

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestPrintTrustedTaskCommands_AllLanguages is the regression test for the
// full-disclosure fix: printTrustedTaskCommands must list every language's
// project-supplied task commands (the exact set the trust hash binds via
// config.ProjectTaskCommands), not just the currently-detected language's
// slots. A project configuring both "go" and "python" task commands must show
// both, even though only one of them is the workspace's detected language.
func TestPrintTrustedTaskCommands_AllLanguages(t *testing.T) {
	ws := t.TempDir()
	if err := config.SetProjectValue(ws, []string{"tasks", "go", "build"}, "go build ./..."); err != nil {
		t.Fatal(err)
	}
	if err := config.SetProjectValue(ws, []string{"tasks", "go", "test"}, "go test ./..."); err != nil {
		t.Fatal(err)
	}
	if err := config.SetProjectValue(ws, []string{"tasks", "python", "lint"}, "ruff check ."); err != nil {
		t.Fatal(err)
	}
	// An inline-interpreter command should still get the warning line.
	if err := config.SetProjectValue(ws, []string{"tasks", "python", "test"}, "bash -c 'pytest'"); err != nil {
		t.Fatal(err)
	}

	cmds, err := config.ProjectTaskCommands(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 4 {
		t.Fatalf("ProjectTaskCommands returned %d entries, want 4: %v", len(cmds), cmds)
	}

	out := captureStdout(t, func() { printTrustedTaskCommands(ws, cmds) })

	// Both languages must appear, even though only "go" (say) is detected.
	if !strings.Contains(out, "[go]") {
		t.Errorf("output missing the go language group:\n%s", out)
	}
	if !strings.Contains(out, "[python]") {
		t.Errorf("output missing the python language group — this is the bug: only the detected language's slots were shown:\n%s", out)
	}
	if !strings.Contains(out, "go build ./...") || !strings.Contains(out, "go test ./...") {
		t.Errorf("output missing go's commands:\n%s", out)
	}
	if !strings.Contains(out, "ruff check .") {
		t.Errorf("output missing python's lint command:\n%s", out)
	}
	if !strings.Contains(out, "bash -c 'pytest'") {
		t.Errorf("output missing python's test command:\n%s", out)
	}
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "bash") {
		t.Errorf("output missing the inline-interpreter warning for python's test command:\n%s", out)
	}
}

// TestPrintTrustedTaskCommands_VerifyIsComposite confirms the verify slot
// still renders as the composite placeholder, not a literal command string,
// and that an empty command set prints nothing.
func TestPrintTrustedTaskCommands_VerifyIsComposite(t *testing.T) {
	ws := t.TempDir()
	if err := config.SetProjectValue(ws, []string{"tasks", "go", "verify"}, "anything"); err != nil {
		t.Fatal(err)
	}
	cmds, err := config.ProjectTaskCommands(ws)
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { printTrustedTaskCommands(ws, cmds) })
	if !strings.Contains(out, "(composite: build then test)") {
		t.Errorf("verify slot should render as the composite placeholder:\n%s", out)
	}
	if strings.Contains(out, "anything") {
		t.Errorf("verify slot must not print its raw stored value:\n%s", out)
	}

	empty := captureStdout(t, func() { printTrustedTaskCommands(ws, nil) })
	if empty != "" {
		t.Errorf("printTrustedTaskCommands(nil) should print nothing, got %q", empty)
	}
}
