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
		return tools.TaskCommand{Slot: slot}, nil // no command configured; the tool reports it
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
	return tools.TaskCommand{Slot: slot, Steps: steps, Provenance: provenance, WorkingDir: workdir}, nil
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
