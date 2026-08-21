package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// The five fixed verbs are registered at package init from the built-in list, so
// a project-defined slot can never have one. This is the verb that reaches it.
func TestTaskSlotCmd_IsRegisteredAlongsideTheBuiltinVerbs(t *testing.T) {
	names := make([]string, 0, len(taskCmds))
	for _, c := range taskCmds {
		names = append(names, strings.Fields(c.Use)[0])
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"build", "lint", "test", "e2e", "verify", "task"} {
		if !strings.Contains(joined, want) {
			t.Errorf("command %q is not registered; got %v", want, names)
		}
	}
}

func TestTaskSlotCmd_RejectsMalformedSlotName(t *testing.T) {
	for _, bad := range []string{"Bad Name", "9build", "check!"} {
		cmd := taskSlotCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{bad})
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		err := cmd.Execute()
		if err == nil {
			t.Errorf("slot %q must be refused", bad)
		} else if !strings.Contains(err.Error(), "not a valid slot name") {
			t.Errorf("slot %q: unexpected error %v", bad, err)
		}
	}
}

// A bare `plumb task` teaches the workspace's vocabulary — including slots the
// project defined, which is the whole reason this verb exists.
func TestWriteTaskSlotListing_IncludesProjectDefinedSlots(t *testing.T) {
	tc := config.TasksConfig{
		Build: "pnpm run build",
		Test:  "pnpm run test",
		Extra: map[string]string{"check": "pnpm run check", "audit": "pnpm audit --prod"},
	}
	var out bytes.Buffer
	if err := writeTaskSlotListing(&out, "/ws", "typescript", tc); err != nil {
		t.Fatalf("writeTaskSlotListing: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"typescript", "/ws",
		"build", "pnpm run build",
		"check", "pnpm run check",
		"audit", "pnpm audit --prod",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("listing should mention %q, got:\n%s", want, got)
		}
	}
	// Built-ins first, then the project's own, sorted.
	if i, j := strings.Index(got, "audit"), strings.Index(got, "check"); i > j {
		t.Errorf("extras should be sorted (audit before check), got:\n%s", got)
	}
	if i, j := strings.Index(got, "build"), strings.Index(got, "audit"); i > j {
		t.Errorf("built-ins should precede extras, got:\n%s", got)
	}
}

// verify is a composite the runner synthesises, so Get returns "" for it.
// Printing that leaves a slot looking unconfigured in the listing that exists to
// say it is runnable.
func TestWriteTaskSlotListing_VerifyIsNotBlank(t *testing.T) {
	var out bytes.Buffer
	tc := config.TasksConfig{Build: "go build ./...", Test: "go test ./..."}
	if err := writeTaskSlotListing(&out, "/ws", "go", tc); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "verify") && strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "verify")) == "" {
			t.Errorf("verify rendered with no command, got line %q", line)
		}
	}
	if !strings.Contains(out.String(), "composite") {
		t.Errorf("verify should be labelled a composite, got:\n%s", out.String())
	}
}

func TestWriteTaskSlotListing_NoCommandsIsAnActionableError(t *testing.T) {
	err := writeTaskSlotListing(&bytes.Buffer{}, "/ws", "zig", config.TasksConfig{})
	if err == nil {
		t.Fatal("a language with no commands should error")
	}
	if !strings.Contains(err.Error(), "[tasks.zig]") {
		t.Errorf("the error should name the config key to set, got: %v", err)
	}
}
