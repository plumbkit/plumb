package cli

// conn_tasks.go wires the run_task tool to the session: it resolves a slot to a
// runnable command for the workspace's primary language and applies the
// per-workspace trust gate to project-supplied commands. Mirrors the gitPolicy
// closure pattern (config adapted into a plain tools type at the cli seam).

import (
	"errors"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// taskResolver resolves slot (+ optional target) to a runnable command for this
// session's workspace and primary language. Default- and global-supplied
// commands always run; a command the project's .plumb/config.toml overrides
// must be trusted first (plumb trust).
func (s *connSession) taskResolver(slot, target string) (tools.TaskCommand, error) {
	ws := s.workspace()
	lang := s.view().acquiredLanguage
	if ws == "" || lang == "" || lang == "none" {
		return tools.TaskCommand{}, errors.New("run_task: no language detected for this workspace; configure [tasks.<lang>] and attach a language")
	}
	tc := s.view().tasks[lang]
	steps, err := buildTaskSteps(tc, slot, target)
	if err != nil {
		return tools.TaskCommand{}, err
	}
	if len(steps) == 0 {
		// No command for this slot. Hand back the context the tool needs to say
		// WHICH language it resolved for and what that language does have, rather
		// than a bare "not configured for this workspace".
		return tools.TaskCommand{Slot: slot, Language: lang, Configured: configuredSlots(tc)}, nil
	}
	workdir, err := commandWorkdir(ws, tc.WorkingDir)
	if err != nil {
		return tools.TaskCommand{}, fmt.Errorf("run_task %s: %w", slot, err)
	}
	provenance, fromProject := taskProvenance(ws, lang, slot)
	if fromProject {
		cmds, err := config.ProjectTaskCommands(ws)
		if err != nil {
			return tools.TaskCommand{}, fmt.Errorf("run_task: reading project task commands: %w", err)
		}
		if !config.NewTrustStore().IsTrustedForTasks(ws, cmds) {
			return tools.TaskCommand{}, fmt.Errorf(
				"run_task: the %s command for %s comes from this project's .plumb/config.toml and is not trusted "+
					"(or the project's task commands changed since `plumb trust` was last run). "+
					"review them, then run `plumb trust` in %s to allow this project's task commands", slot, lang, ws)
		}
	}
	return tools.TaskCommand{
		Slot: slot, Steps: steps, Provenance: provenance, WorkingDir: workdir,
		Language: lang, Configured: configuredSlots(tc),
	}, nil
}

// taskState reports the resolved run_task / run_command surface for this
// session, for the session_start orientation section. It answers from the same
// view the resolvers read, so the report and the behaviour cannot disagree.
//
// Unreachable is the point of it: task resolution keys on the single primary
// language, so in a monorepo the other detected languages' commands — including
// the shipped defaults — cannot be run through run_task at all. That was
// invisible, while the identity line happily listed every language.
func (s *connSession) taskState() tools.TaskState {
	v := s.view()
	lang := v.acquiredLanguage
	if lang == "none" {
		lang = ""
	}
	st := tools.TaskState{Language: lang}
	if lang != "" {
		st.Configured = configuredSlots(v.tasks[lang])
	}
	for _, other := range v.discoveredLangs {
		if other != lang && other != "" && other != "none" {
			st.Unreachable = append(st.Unreachable, other)
		}
	}
	for _, c := range v.commands {
		st.Commands = append(st.Commands, c.Name)
	}
	return st
}

// targetAcceptanceProbe is a syntactically valid target used only to ask whether
// the test command has a slot for one. It is never run.
const targetAcceptanceProbe = "./..."

// testScope reports how this session's workspace runs a SCOPED test command, so
// topology_affected can emit a target run_task will accept instead of assuming
// `go test`. A session with no language attached yields the zero value, which
// the tool renders as bare directories and no command.
//
// The language rule lives HERE, not in internal/tools, because this is the only
// layer that can see both the language and the configured command.
func (s *connSession) testScope() tools.TestScope {
	v := s.view()
	lang := v.acquiredLanguage
	if lang == "" || lang == LanguageNone {
		return tools.TestScope{}
	}
	tc := v.tasks[lang]
	return tools.TestScope{
		Language:   lang,
		WorkingDir: tc.WorkingDir,
		Style:      testTargetStyle(lang, tc),
	}
}

// testTargetStyle decides how a package directory should be spelled for this
// workspace's test command, or TargetNone when it cannot be spelled safely.
//
// Two conditions, and BOTH are needed. The command must take a positional path
// operand (testSlotTakesPositionalTarget), and the language's runner must treat
// that operand as a PATH. Checking only the first is what let
// `[tasks.go] test = "go test -run {target}"` through: the probe proves a
// {target} slot exists, never that it means a directory, so the emitted package
// path landed in a test-NAME regex, matched nothing, and exited 0 — a silent
// green over zero tests, which is worse than the hardcoded `go test` this
// replaced. Checking only the second mis-handles a project that rewired its own
// command.
//
// rust is excluded even though `cargo test <filter>` is positional, because the
// filter matches test NAMES. typescript, swift and zig ship no placeholder and
// scope through project-specific flags.
func testTargetStyle(lang string, tc config.TasksConfig) tools.TargetStyle {
	if !testSlotTakesPositionalTarget(tc) {
		return tools.TargetNone
	}
	switch lang {
	case "go":
		return tools.TargetGoPackage
	case "python":
		return tools.TargetPath
	default:
		return tools.TargetNone
	}
}

// testSlotTakesPositionalTarget reports whether the test command has a {target}
// placeholder that is a positional OPERAND rather than the value of a flag.
//
// Acceptance is asked of buildTaskSteps — the same function run_task uses —
// rather than re-derived, because configuredSlots below records what happens
// otherwise: two hand-written predicates disagreed with buildTaskSteps in
// opposite directions. The len(steps) check matters for the same reason it does
// there — an unset or whitespace-only command parses to a nil argv and returns
// (nil, nil), so err == nil alone reports "takes a target" for a workspace that
// has no test command at all.
//
// Position is then checked directly, since no existing function answers it.
func testSlotTakesPositionalTarget(tc config.TasksConfig) bool {
	steps, err := buildTaskSteps(tc, "test", targetAcceptanceProbe)
	if err != nil || len(steps) == 0 {
		return false
	}
	argv, err := config.ParseTaskCommand(tc.Get("test"))
	if err != nil {
		return false
	}
	return placeholderIsPositionalOperand(argv)
}

// placeholderIsPositionalOperand reports whether argv's {target} element is a
// trailing positional operand.
//
// A placeholder preceded by a flag that takes a separate value (`-run {target}`,
// `-k {target}`) is that flag's VALUE, and a directory handed to it selects
// nothing while still exiting 0. A flag carrying its own value (`-count=1`) does
// not consume what follows, so `go test -count=1 {target:./...}` is positional —
// treating every leading dash as consuming would refuse a perfectly good
// command.
//
// Deliberately conservative beyond that: a placeholder that is not last
// (`go test {target} -v`) is treated as unknown rather than guessed at, because
// naming the directory costs a caller one edit while a wrong target costs them a
// green run that tested nothing.
func placeholderIsPositionalOperand(argv []string) bool {
	for i, a := range argv {
		if _, ok := targetPlaceholder(a); !ok {
			continue
		}
		if i == 0 || consumesNextArg(argv[i-1]) {
			return false
		}
		return i == len(argv)-1
	}
	return false
}

// consumesNextArg reports whether an argv element is a flag whose value is the
// FOLLOWING element. A flag spelled with "=" carries its own value and does not.
func consumesNextArg(arg string) bool {
	return strings.HasPrefix(arg, "-") && !strings.Contains(arg, "=")
}

// configuredSlots lists the task slots that actually have a command, in a fixed
// order.
//
// It answers by asking buildTaskSteps — the same function run_task uses — rather
// than by re-deriving the condition. Two hand-written predicates disagreed with
// it in opposite directions: `tc.Build != "" && tc.Test != ""` was stricter than
// the verify branch, which happily runs whichever half is set, so a
// build-only config reported "verify: not configured" for a call that in fact
// runs; and `tc.Get(slot) != ""` was looser than ParseTaskCommand, which trims
// first, so a whitespace-only command reported as configured for a call that is
// refused. A session_start section that contradicts the tool it describes is
// worse than no section, so there is now one source of truth.
func configuredSlots(tc config.TasksConfig) []string {
	var out []string
	for _, slot := range []string{"build", "lint", "test", "e2e", "verify"} {
		steps, err := buildTaskSteps(tc, slot, "")
		if err != nil || len(steps) == 0 {
			continue
		}
		out = append(out, slot)
	}
	return out
}

// buildTaskSteps turns a slot into the argv steps to run. verify is the
// composite build-then-test; every other slot is a single command.
func buildTaskSteps(tc config.TasksConfig, slot, target string) ([][]string, error) {
	if slot == "verify" {
		var steps [][]string
		for _, sub := range []string{"build", "test"} {
			argv, err := taskStep(tc, sub, "")
			if err != nil {
				return nil, err
			}
			if argv != nil {
				steps = append(steps, argv)
			}
		}
		return steps, nil
	}
	argv, err := taskStep(tc, slot, target)
	if err != nil {
		return nil, err
	}
	if argv == nil {
		return nil, nil
	}
	return [][]string{argv}, nil
}

// taskStep parses one slot's command into an argv and applies the {target}
// substitution. A nil argv means the slot is unset.
func taskStep(tc config.TasksConfig, slot, target string) ([]string, error) {
	argv, err := config.ParseTaskCommand(tc.Get(slot))
	if err != nil {
		return nil, err
	}
	if argv == nil {
		return nil, nil
	}
	return substituteTarget(argv, target)
}

// substituteTarget replaces a {target} argv element with target. A target with
// no placeholder is an error; a placeholder with no target is an error UNLESS
// the placeholder declares a default, written {target:<default>}.
//
// The inline default is what makes scoping reachable out of the box. Every
// shipped test command was `go test ./...` — no placeholder — so run_task's
// target and mutation_test's test_target were not merely undiscoverable but an
// outright error on an unmodified install, and a mutation run therefore paid for
// the WHOLE suite per mutant.
//
// Why a default attached to the placeholder rather than one held elsewhere: the
// two alternatives both misfire. A per-language fallback applied to a bare
// {target} would silently change the meaning of commands users already wrote —
// `go test -run {target}` would quietly run everything instead of refusing — and
// a separate "scoped" slot doubles the config surface for one argument. Written
// inline, the default sits where the placeholder is and cannot drift from it,
// and a bare {target} keeps its strict contract exactly as before, so no
// existing configuration changes behaviour.
//
// An EMPTY default ({target:}) drops the element entirely. That is not a
// curiosity: for cargo test, swift test and friends, "everything" is the ABSENCE
// of a positional argument, so there is no string that could stand in for it.
func substituteTarget(argv []string, target string) ([]string, error) {
	out := make([]string, 0, len(argv))
	found := false
	for _, a := range argv {
		def, ok := targetPlaceholder(a)
		if !ok {
			out = append(out, a)
			continue
		}
		found = true
		value := target
		if value == "" {
			if def == nil {
				return nil, errors.New("this command needs a target ({target} placeholder)")
			}
			value = *def
		}
		if value == "" {
			continue // an empty default means "omit this argument"
		}
		out = append(out, value)
	}
	if target != "" && !found {
		return nil, errors.New("a target was given but the command has no {target} placeholder")
	}
	return out, nil
}

// targetPlaceholder recognises `{target}` and `{target:<default>}` as WHOLE argv
// elements, returning the declared default (nil when there is none).
//
// Whole-element only, matching what the plain token always accepted: the argv is
// split on whitespace with no quoting, so a placeholder glued to other text
// would substitute into an argument whose boundaries nobody can see.
func targetPlaceholder(arg string) (def *string, ok bool) {
	if arg == config.TargetToken {
		return nil, true
	}
	if !config.IsTargetToken(arg) {
		return nil, false
	}
	value := arg[len(config.TargetTokenPrefix) : len(arg)-1]
	return &value, true
}

// taskProvenance reports the layer a slot's command comes from and whether the
// project overrides it (so the trust gate applies). verify consults build+test.
// It asks ProjectTaskCommands — the same enumeration the trust hash binds to —
// rather than looking the key up by name, and compares case-INSENSITIVELY.
//
// The old version called config.ProjectValuePresent(ws, {"tasks", lang, slot}),
// an exact map lookup, and that was a gate bypass. go-toml/v2 binds a table name
// to a struct field case-insensitively, so a cloned repository shipping
// `[TASKS.go] test = "..."` had its command decoded into Config.Tasks and run by
// run_task, while the exact lookup missed the raw key: fromProject came back
// false and the trust check below was skipped entirely. The command was also
// absent from ProjectTaskCommands, so it was never in the hash either — both
// halves of the defence missed the same spelling. Arbitrary code execution from
// an untrusted clone, and the sibling of the [[COMMAND]] bypass in
// conn_commands.go.
//
// A read error fails CLOSED (treated as project-supplied, so the trust gate
// applies). A gate that cannot determine provenance must not assume the safe
// answer.
func taskProvenance(ws, lang, slot string) (label string, fromProject bool) {
	slots := []string{slot}
	if slot == "verify" {
		slots = []string{"build", "test"}
	}
	cmds, err := config.ProjectTaskCommands(ws)
	if err != nil {
		return "project", true
	}
	for _, c := range cmds {
		if !strings.EqualFold(c.Lang, lang) {
			continue
		}
		// A project working_dir makes EVERY slot of that language project-supplied,
		// including the ones whose command comes from the shipped defaults.
		//
		// Without this the gate has a hole the size of the feature: working_dir is
		// not a command, so a project that overrides no command at all still
		// decides WHERE the default `go build ./...` runs. Provenance answers "did
		// this project influence what is about to execute?", and choosing the
		// directory is influence — the argv is only half of what runs.
		if strings.EqualFold(c.Slot, taskWorkingDirKey) {
			return "project", true
		}
		for _, sl := range slots {
			if strings.EqualFold(c.Slot, sl) {
				return "project", true
			}
		}
	}
	return "config", false
}

// taskWorkingDirKey is the [tasks.<lang>] key that is not a command slot.
// ProjectTaskCommands sweeps every string under the table, so it arrives as a
// TaskCommandSpec with this Slot — which is what puts it in the trust hash, and
// what taskProvenance matches on above.
const taskWorkingDirKey = "working_dir"
