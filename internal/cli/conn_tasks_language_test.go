package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// newTaskLanguageSession builds a session pinned to ws whose PRIMARY language is
// primary, carrying the given per-language task blocks — the polyglot shape
// run_task's language argument exists for.
func newTaskLanguageSession(t *testing.T, ws, primary string, tasks map[string]config.TasksConfig) *connSession {
	t.Helper()
	s := &connSession{
		store: config.NewStore(config.Defaults()),
		ctx:   context.Background(),
	}
	s.mutate(func(v *sessionView) {
		v.acquiredRoot = ws
		v.acquiredLanguage = primary
		v.tasks = tasks
	})
	return s
}

// TestTaskResolver_LanguageReachesASecondaryBlock is the whole point of the
// argument: a repo that resolved as one language can still run another's
// commands. Before this, resolution was a single map lookup on the primary, so
// the sibling language's commands — shipped defaults included — could not be
// run through run_task at all, and the agent was told to use the shell.
func TestTaskResolver_LanguageReachesASecondaryBlock(t *testing.T) {
	ws := t.TempDir()
	tasks := map[string]config.TasksConfig{
		"typescript": {Test: "pnpm test"},
		"python":     {Test: "pytest"},
	}
	s := newTaskLanguageSession(t, ws, "typescript", tasks)

	cmd, err := s.taskResolver("test", "", "python")
	if err != nil {
		t.Fatalf("resolving the python test command: %v", err)
	}
	if cmd.Language != "python" {
		t.Errorf("Language = %q, want python", cmd.Language)
	}
	if len(cmd.Steps) == 0 || cmd.Steps[0][0] != "pytest" {
		t.Errorf("Steps = %v, want the pytest command", cmd.Steps)
	}
}

// TestTaskResolver_EmptyLanguageStillMeansPrimary pins the default, which every
// existing caller depends on — mutation_test passes "" precisely to get it.
func TestTaskResolver_EmptyLanguageStillMeansPrimary(t *testing.T) {
	ws := t.TempDir()
	tasks := map[string]config.TasksConfig{
		"typescript": {Test: "pnpm test"},
		"python":     {Test: "pytest"},
	}
	s := newTaskLanguageSession(t, ws, "typescript", tasks)

	cmd, err := s.taskResolver("test", "", "")
	if err != nil {
		t.Fatalf("resolving the primary test command: %v", err)
	}
	if cmd.Language != "typescript" {
		t.Errorf("Language = %q, want the primary (typescript)", cmd.Language)
	}
}

// TestTaskResolver_UnknownLanguageRefusedWithTheList refuses rather than
// silently falling back to the primary — a fallback would run the WRONG
// language's tests and report success.
func TestTaskResolver_UnknownLanguageRefusedWithTheList(t *testing.T) {
	ws := t.TempDir()
	tasks := map[string]config.TasksConfig{
		"typescript": {Test: "pnpm test"},
		"python":     {Test: "pytest"},
	}
	s := newTaskLanguageSession(t, ws, "typescript", tasks)

	_, err := s.taskResolver("test", "", "ruby")
	if err == nil {
		t.Fatal("expected a refusal for a language with no task commands")
	}
	for _, want := range []string{"ruby", "python", "typescript"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q should name %q", err, want)
		}
	}
}

// TestTaskResolver_LanguageWithNoConfiguredSlotsIsRefused covers the gap between
// "the key exists in the map" and "it can actually run something". An empty
// block must be refused with the same list, not accepted into a resolution that
// then reports an unconfigured slot for a language the caller never asked about.
func TestTaskResolver_LanguageWithNoConfiguredSlotsIsRefused(t *testing.T) {
	ws := t.TempDir()
	tasks := map[string]config.TasksConfig{
		"typescript": {Test: "pnpm test"},
		"html":       {}, // present, entirely empty — the reported failure shape
	}
	s := newTaskLanguageSession(t, ws, "typescript", tasks)

	_, err := s.taskResolver("test", "", "html")
	if err == nil {
		t.Fatal("expected a refusal for a language whose block configures no slots")
	}
	if !strings.Contains(err.Error(), "typescript") {
		t.Errorf("refusal %q should name the languages that DO have commands", err)
	}
}

// TestTaskResolver_NoPrimaryNamesTheLanguagesYouCouldAskFor: with no language
// attached, an unqualified call cannot resolve — but the remedy is now an
// argument the caller can pass, not only "attach a language".
func TestTaskResolver_NoPrimaryNamesTheLanguagesYouCouldAskFor(t *testing.T) {
	ws := t.TempDir()
	tasks := map[string]config.TasksConfig{"python": {Test: "pytest"}}
	s := newTaskLanguageSession(t, ws, LanguageNone, tasks)

	_, err := s.taskResolver("test", "", "")
	if err == nil {
		t.Fatal("expected a refusal when no language is attached")
	}
	if !strings.Contains(err.Error(), "python") {
		t.Errorf("refusal %q should name python as a language you could ask for", err)
	}

	// ...and naming it explicitly works even with no primary attached.
	cmd, err := s.taskResolver("test", "", "python")
	if err != nil {
		t.Fatalf("explicit language with no primary attached: %v", err)
	}
	if cmd.Language != "python" {
		t.Errorf("Language = %q, want python", cmd.Language)
	}
}
