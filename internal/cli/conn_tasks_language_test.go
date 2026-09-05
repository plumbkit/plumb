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

// TestTaskResolver_LanguageDoesNotBypassTheTrustGate is the security-relevant
// property of the language argument, and it had no test until an adversarial
// review pointed that out. Reaching a NON-primary language's commands must not
// be a way around `plumb trust`: the gate keys on the RESOLVED language and
// hashes the whole project command set, so it has to behave identically whether
// the language came from detection or from the caller.
func TestTaskResolver_LanguageDoesNotBypassTheTrustGate(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // isolate trust.json
	ws := t.TempDir()
	// A project-supplied command for a NON-primary language.
	if err := config.SetProjectValue(ws, []string{"tasks", "python", "test"}, "pytest -x"); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]config.TasksConfig{
		"go":     {Test: "go test ./..."},
		"python": {Test: "pytest -x"},
	}
	s := newTaskLanguageSession(t, ws, "go", tasks)

	_, err := s.taskResolver("test", "", "python")
	if err == nil {
		t.Fatal("an untrusted project command must be refused even when reached via `language`")
	}
	if !strings.Contains(err.Error(), "not trusted") {
		t.Errorf("refusal %q should name the trust gate", err)
	}

	// Trusting the project's command set lets it through — proving the refusal
	// above was the gate doing its job, not the language argument failing.
	cmds, err := config.ProjectTaskCommands(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.NewTrustStore().SetTrustedForProject(ws, cmds, nil); err != nil {
		t.Fatal(err)
	}
	cmd, err := s.taskResolver("test", "", "python")
	if err != nil {
		t.Fatalf("after `plumb trust`, the non-primary command must run: %v", err)
	}
	if cmd.Language != "python" {
		t.Errorf("Language = %q, want python", cmd.Language)
	}
}

// TestLanguagesWithCommands_OmitsUnrequestableKeys: config map keys are not
// case-folded, so [tasks.Python] is a distinct entry that run_task's shape
// check refuses. The remedy list must not advertise a key that bounces.
func TestLanguagesWithCommands_OmitsUnrequestableKeys(t *testing.T) {
	ws := t.TempDir()
	tasks := map[string]config.TasksConfig{
		"python": {Test: "pytest"},
		"Python": {Test: "pytest"}, // a config typo, not a second language
	}
	s := newTaskLanguageSession(t, ws, "go", tasks)

	got := languagesWithCommands(s.view())
	if !strings.Contains(got, "python") {
		t.Errorf("remedy %q should list the requestable key", got)
	}
	if strings.Contains(got, "Python") {
		t.Errorf("remedy %q must not advertise a key run_task would refuse for shape", got)
	}
}
