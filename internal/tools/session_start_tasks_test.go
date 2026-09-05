package tools

import (
	"strings"
	"testing"
)

func renderTaskSection(t *testing.T, st TaskState) string {
	t.Helper()
	tool := &SessionStart{tasksFn: func() TaskState { return st }}
	var sb strings.Builder
	tool.writeSessionTasks(&sb, "/ws")
	return sb.String()
}

// TestWriteSessionTasks_ReportsEmptySlots is the core of the F3 fix: an agent
// must learn at orientation that run_task has nothing to run, instead of being
// told to "prefer it over shelling out" and finding out via a refused call.
func TestWriteSessionTasks_ReportsEmptySlots(t *testing.T) {
	out := renderTaskSection(t, TaskState{Language: "zig"})
	if !strings.Contains(out, "no commands configured for zig") {
		t.Errorf("expected the empty-slot report, got:\n%s", out)
	}
	if !strings.Contains(out, "[tasks.zig]") {
		t.Errorf("expected the remedy to name the config key, got:\n%s", out)
	}
	if !strings.Contains(out, "agent_config") {
		t.Errorf("expected the remedy to mention agent_config, got:\n%s", out)
	}
}

// TestWriteSessionTasks_NamesUnreachableLanguages pins the finding that made F3
// more than a config gap: an unqualified run_task keys on the single primary
// language, so a monorepo's other languages go unnoticed even though their
// defaults exist and the identity line lists them.
//
// The assertion changed with the capability. It used to require the words
// "primary language only", which was the whole story while those commands could
// not be run at all; the section then told the agent to use the shell for them.
// run_task's `language` argument reaches them, so the section must hand over
// that argument instead — and this test now pins the routing hint, because a
// section that still said "use the shell" would send agents out of the tool for
// work it can do.
func TestWriteSessionTasks_NamesUnreachableLanguages(t *testing.T) {
	out := renderTaskSection(t, TaskState{
		Language:    "zig",
		Configured:  []string{"build", "test"},
		Unreachable: []string{"typescript"},
	})
	if !strings.Contains(out, "typescript") {
		t.Errorf("expected the sibling language to be named, got:\n%s", out)
	}
	if !strings.Contains(out, `language: "typescript"`) {
		t.Errorf("expected the section to hand over the argument that reaches it, got:\n%s", out)
	}
	if strings.Contains(out, "use the shell") {
		t.Errorf("the section must no longer send agents to the shell for a language run_task can reach, got:\n%s", out)
	}
}

// TestWriteSessionTasks_ListsConfiguredAndMissing: when commands DO exist, the
// section must still say which slots will be refused.
func TestWriteSessionTasks_ListsConfiguredAndMissing(t *testing.T) {
	out := renderTaskSection(t, TaskState{Language: "go", Configured: []string{"build", "test", "verify"}})
	if !strings.Contains(out, "build, test, verify") {
		t.Errorf("expected the configured slots, got:\n%s", out)
	}
	if !strings.Contains(out, "Not configured: lint, e2e") {
		t.Errorf("expected the missing slots named, got:\n%s", out)
	}
}

// TestWriteSessionTasks_ReportsEmptyCommandAllowList: run_command ships with no
// [[command]] entries at all, so a fresh workspace cannot use it either.
func TestWriteSessionTasks_ReportsEmptyCommandAllowList(t *testing.T) {
	out := renderTaskSection(t, TaskState{Language: "go", Configured: []string{"build"}})
	if !strings.Contains(out, "no `[[command]]` entries configured") {
		t.Errorf("expected the empty allow-list report, got:\n%s", out)
	}
	withCmds := renderTaskSection(t, TaskState{Language: "go", Configured: []string{"build"}, Commands: []string{"e2e"}})
	if !strings.Contains(withCmds, "`run_command`: e2e") {
		t.Errorf("expected the configured commands listed, got:\n%s", withCmds)
	}
}

// TestWriteSessionTasks_NilSafe: the section must vanish when unwired, the same
// way every other injected section does.
func TestWriteSessionTasks_NilSafe(t *testing.T) {
	var sb strings.Builder
	(&SessionStart{}).writeSessionTasks(&sb, "/ws")
	if sb.String() != "" {
		t.Errorf("unwired tasksFn must emit nothing, got:\n%s", sb.String())
	}
	sb.Reset()
	tool := &SessionStart{tasksFn: func() TaskState { return TaskState{} }}
	tool.writeSessionTasks(&sb, "")
	if sb.String() != "" {
		t.Errorf("empty workspace must emit nothing, got:\n%s", sb.String())
	}
	sb.Reset()
	tool.writeSessionTasks(&sb, "/ws")
	if sb.String() != "" {
		t.Errorf("a wholly empty TaskState must emit nothing, got:\n%s", sb.String())
	}
}

func TestMissingTaskSlots(t *testing.T) {
	got := missingTaskSlots([]string{"build", "test"})
	want := []string{"lint", "e2e", "verify"}
	if len(got) != len(want) {
		t.Fatalf("missingTaskSlots = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("missingTaskSlots[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if n := len(missingTaskSlots(allTaskSlots)); n != 0 {
		t.Errorf("a fully configured language should report nothing missing, got %d", n)
	}
}

// TestNoCommandError_NamesLanguageAndSlots pins the improved run_task refusal.
// The old text ("no test command configured for this workspace") named neither
// the language it resolved for nor what that language does have.
func TestNoCommandError_NamesLanguageAndSlots(t *testing.T) {
	err := noCommandError(TaskCommand{Language: "typescript", Configured: []string{"build", "test"}}, "lint")
	msg := err.Error()
	for _, want := range []string{"typescript", "build, test", "[tasks.typescript]", "agent_config"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected %q in the refusal, got: %s", want, msg)
		}
	}

	// And the degenerate case, where nothing at all is configured.
	bare := noCommandError(TaskCommand{Language: "zig"}, "lint").Error()
	if !strings.Contains(bare, "no slots are configured") {
		t.Errorf("expected the no-slots phrasing, got: %s", bare)
	}
	// A resolver that supplied no context must still produce a usable message.
	// The earlier assertion here only checked for "this workspace", which was
	// satisfied while the REMEDY line read `[tasks.this workspace]` — invalid TOML
	// and not actionable. Assert the remedy specifically.
	unknown := noCommandError(TaskCommand{}, "build").Error()
	if !strings.Contains(unknown, "this workspace") {
		t.Errorf("expected a graceful subject with no language, got: %s", unknown)
	}
	if strings.Contains(unknown, "[tasks.this workspace]") {
		t.Errorf("the remedy must not interpolate the prose subject as a config key, got: %s", unknown)
	}
	if !strings.Contains(unknown, "[tasks.<lang>]") {
		t.Errorf("expected a placeholder config key when the language is unknown, got: %s", unknown)
	}
}

// The four combinations of (primary set?) x (siblings detected?). A re-review
// found the no-primary branch entirely unpinned: the only test that set
// Unreachable also set Language, so the branch added to fix the "language ()"
// rendering could be reverted with the whole package green.
func TestWriteSessionTasks_NoPrimaryNamesOnlyRunnableLanguages(t *testing.T) {
	// Detection found java and typescript; only typescript has commands. The
	// hint must name typescript — naming java would hand the agent an argument
	// run_task refuses, which is the dead end this section exists to avoid.
	out := renderTaskSection(t, TaskState{
		Language:    "",
		Unreachable: []string{"java", "typescript"},
		Runnable:    []string{"typescript"},
	})
	if !strings.Contains(out, `language: "typescript"`) {
		t.Errorf("expected the hint to name a language that HAS commands, got:\n%s", out)
	}
	if strings.Contains(out, `language: "java"`) {
		t.Errorf("must not hand over a detected language with no commands, got:\n%s", out)
	}
	if strings.Contains(out, "is unavailable") {
		t.Errorf("run_task is NOT unavailable here — an explicit language resolves, got:\n%s", out)
	}
	if !strings.Contains(out, "java") {
		t.Errorf("the detected siblings should still be listed, got:\n%s", out)
	}
	if strings.Contains(out, "language ()") {
		t.Errorf("empty primary must never be rendered, got:\n%s", out)
	}
}

// The truth regression the same review caught: with nothing runnable, the old
// "run_task is unavailable" was TRUE, and the fix must not replace it with a
// claim that some language can be named.
func TestWriteSessionTasks_NoPrimaryAndNothingRunnableStaysHonest(t *testing.T) {
	out := renderTaskSection(t, TaskState{
		Language:    "",
		Unreachable: []string{"java"}, // detected, but no [tasks.java] commands
		Runnable:    nil,
	})
	if !strings.Contains(out, "is unavailable") {
		t.Errorf("with nothing runnable the section must say so, got:\n%s", out)
	}
	if strings.Contains(out, "name one with") {
		t.Errorf("must not claim a language can be named when none has commands, got:\n%s", out)
	}
	if strings.Contains(out, "language ()") {
		t.Errorf("empty primary must never be rendered, got:\n%s", out)
	}
}

// Neither a primary nor siblings: unchanged, and still the honest message.
func TestWriteSessionTasks_NoPrimaryNoSiblings(t *testing.T) {
	out := renderTaskSection(t, TaskState{Language: "", Commands: []string{"fmt"}})
	if !strings.Contains(out, "is unavailable") {
		t.Errorf("expected the unavailable message, got:\n%s", out)
	}
}
