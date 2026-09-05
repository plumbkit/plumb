package tools

import (
	"fmt"
	"strings"
)

// allTaskSlots is the BUILT-IN slot set, in report order — the set "not
// configured" is reported against. A project-defined extra slot is deliberately
// absent: it is opt-in, so it can never be missing, and listing every slot a
// project did not define would turn the orientation line into noise. Extras
// still appear in TaskState.Configured when they have a command.
var allTaskSlots = []string{"build", "lint", "test", "e2e", "verify"}

// TaskState is the resolved task/command surface for this session, reported at
// orientation so an agent learns what run_task can actually do before it tries.
//
// It exists because the guidance told agents to "prefer run_task over shelling
// out" unconditionally, while resolution reads a SINGLE primary language. In a
// multi-language repo the sibling language's commands — shipped defaults
// included — are unreachable, so the advice sent the agent into a rejected call
// and then into raw shell for every build and test.
type TaskState struct {
	Language    string   // the primary language task resolution uses ("" when none)
	Configured  []string // slots with a command for that language
	Unreachable []string // other detected languages, whose tasks run_task cannot reach
	Commands    []string // names in the [[command]] allow-list, for run_command
}

// WithTasks wires the resolved task/command state accessor. Injected rather than
// read here so internal/tools keeps its config dependency at the boundary, the
// same way gitPolicyFn and collabFn do. Nil-safe: unset ⇒ the section is omitted.
func (t *SessionStart) WithTasks(fn func() TaskState) *SessionStart {
	t.tasksFn = fn
	return t
}

// writeSessionTasks reports what run_task and run_command can actually do in
// this workspace, modelled on writeSessionGitPolicy: state the live, resolved
// capability up front rather than let the agent discover it via a rejected call.
//
// The client guidance tells agents to "prefer run_task over shelling out". That
// advice was unconditional while the capability is not — resolution reads a
// single primary language — which is how an agent on a Zig+TypeScript workspace
// ended up running pnpm, playwright and zig through raw shell for a whole
// session. Nil-safe: skipped when unwired.
func (t *SessionStart) writeSessionTasks(sb *strings.Builder, ws string) {
	if t.tasksFn == nil || ws == "" {
		return
	}
	st := t.tasksFn()
	if st.Language == "" && len(st.Commands) == 0 && len(st.Unreachable) == 0 {
		return
	}

	sb.WriteString("## Tasks & commands (live)\n\n")
	writeTaskSlotState(sb, st)
	writeUnreachableLanguages(sb, st)
	writeCommandAllowList(sb, st)
	sb.WriteString("\n")
}

// writeTaskSlotState reports run_task's resolved slots for the primary language.
func writeTaskSlotState(sb *strings.Builder, st TaskState) {
	switch {
	case st.Language == "":
		sb.WriteString("`run_task` is unavailable — no language is attached to this workspace.\n")
	case len(st.Configured) == 0:
		fmt.Fprintf(sb, "`run_task`: **no commands configured for %s** — every slot is empty, so each call is refused.\n", st.Language)
		fmt.Fprintf(sb, "↳ Set them under `[tasks.%s]` in .plumb/config.toml, or ask the user to enable `[agent_config_writes]` and use `agent_config op=set`.\n", st.Language)
	default:
		fmt.Fprintf(sb, "`run_task` (%s): %s. Prefer it over shelling out.\n",
			st.Language, strings.Join(st.Configured, ", "))
		if missing := missingTaskSlots(st.Configured); len(missing) > 0 {
			fmt.Fprintf(sb, "↳ Not configured: %s — those calls are refused.\n", strings.Join(missing, ", "))
		}
	}
}

// writeUnreachableLanguages names the sibling languages whose task commands the
// caller would otherwise not think to ask for. Without this the agent sees
// "Language: Swift, Zig" in the identity line and reasonably assumes an
// unqualified run_task covers both.
//
// The name is now historical: these languages WERE unreachable, because
// resolution was single-primary and the tool had no way to say which language
// it meant. run_task's `language` argument reaches them, so this is a routing
// hint rather than a limitation — and it must say so, because the old wording
// ("use the shell for those") actively sent agents out of the tool, losing the
// no-shell argv contract and the trust gate, for work the tool can now do.
func writeUnreachableLanguages(sb *strings.Builder, st TaskState) {
	if len(st.Unreachable) == 0 {
		return
	}
	fmt.Fprintf(sb, "\nAlso detected here: %s. An unqualified `run_task` resolves against the primary\n",
		strings.Join(st.Unreachable, ", "))
	fmt.Fprintf(sb, "language (%s) — pass `language: \"%s\"` to run one of the others' commands.\n",
		st.Language, st.Unreachable[0])
}

// writeCommandAllowList reports run_command's [[command]] allow-list, which
// ships empty, so the tool is unusable until the user adds an entry.
func writeCommandAllowList(sb *strings.Builder, st TaskState) {
	if len(st.Commands) == 0 {
		sb.WriteString("\n`run_command`: no `[[command]]` entries configured — the allow-list is empty, so every call is refused.\n")
		return
	}
	fmt.Fprintf(sb, "\n`run_command`: %s.\n", strings.Join(st.Commands, ", "))
}

// missingTaskSlots returns the BUILT-IN slots absent from configured,
// preserving the canonical order. configured may contain project-defined extras;
// they are simply not looked for here.
func missingTaskSlots(configured []string) []string {
	have := make(map[string]bool, len(configured))
	for _, s := range configured {
		have[s] = true
	}
	var missing []string
	for _, s := range allTaskSlots {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	return missing
}
