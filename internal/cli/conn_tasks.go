package cli

// conn_tasks.go wires the run_task tool to the session: it resolves a slot to a
// runnable command for the workspace's primary language and applies the
// per-workspace trust gate to project-supplied commands. Mirrors the gitPolicy
// closure pattern (config adapted into a plain tools type at the cli seam).

import (
	"fmt"

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
		return tools.TaskCommand{}, fmt.Errorf("run_task: no language detected for this workspace; configure [tasks.<lang>] and attach a language")
	}
	steps, err := buildTaskSteps(s.view().tasks[lang], slot, target)
	if err != nil {
		return tools.TaskCommand{}, err
	}
	if len(steps) == 0 {
		return tools.TaskCommand{Slot: slot}, nil // no command configured; the tool reports it
	}
	provenance, fromProject := taskProvenance(ws, lang, slot)
	if fromProject && !config.NewTrustStore().IsTrusted(ws) {
		return tools.TaskCommand{}, fmt.Errorf(
			"run_task: the %s command for %s comes from this project's .plumb/config.toml and is not trusted. "+
				"review it, then run `plumb trust` in %s to allow this project's task commands", slot, lang, ws)
	}
	return tools.TaskCommand{Slot: slot, Steps: steps, Provenance: provenance}, nil
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

// substituteTarget replaces a literal {target} argv element with target. A
// target with no placeholder, or a placeholder with no target, is an error.
func substituteTarget(argv []string, target string) ([]string, error) {
	out := make([]string, 0, len(argv))
	found := false
	for _, a := range argv {
		if a == "{target}" {
			found = true
			if target == "" {
				return nil, fmt.Errorf("this command needs a target ({target} placeholder)")
			}
			out = append(out, target)
			continue
		}
		out = append(out, a)
	}
	if target != "" && !found {
		return nil, fmt.Errorf("a target was given but the command has no {target} placeholder")
	}
	return out, nil
}

// taskProvenance reports the layer a slot's command comes from and whether the
// project overrides it (so the trust gate applies). verify consults build+test.
func taskProvenance(ws, lang, slot string) (label string, fromProject bool) {
	slots := []string{slot}
	if slot == "verify" {
		slots = []string{"build", "test"}
	}
	for _, sl := range slots {
		if present, _ := config.ProjectValuePresent(ws, []string{"tasks", lang, sl}); present {
			return "project", true
		}
	}
	return "config", false
}
