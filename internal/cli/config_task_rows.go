package cli

import (
	"slices"

	"charm.land/lipgloss/v2/table"

	"github.com/plumbkit/plumb/internal/config"
)

// config_task_rows.go renders [tasks.<lang>] env for `plumb config show` (#537):
// one tasks.<lang> section per language that has an env, one row per entry with
// the layer it came from. The commands themselves are listed by `plumb task`;
// env is shown here because it is configuration with a provenance, and a
// project-supplied entry changes what every command of the language runs.

// addTaskEnvSections adds a section for every language whose merged env is
// non-empty, in language order.
func addTaskEnvSections(t *table.Table, globalCfg, projectCfg config.Config) {
	langs := make([]string, 0, len(projectCfg.Tasks))
	for lang, tc := range projectCfg.Tasks {
		if len(tc.Env) > 0 {
			langs = append(langs, lang)
		}
	}
	slices.Sort(langs)
	for _, lang := range langs {
		addConfigSection(t, "tasks."+lang, taskEnvConfigRows(lang, globalCfg, projectCfg))
	}
}

// taskEnvConfigRows is one language's env as {key, value, provenance} rows,
// sorted by name. No env ships by default, so the layers are global and project;
// a project-supplied entry says it is trust-gated, since it does not run until
// `plumb trust` however it reads here.
func taskEnvConfigRows(lang string, globalCfg, projectCfg config.Config) [][]string {
	env := projectCfg.Tasks[lang].Env
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	slices.Sort(names)
	rows := make([][]string, 0, len(names))
	for _, k := range names {
		src := sourceFor("env."+k, "", globalCfg.Tasks[lang].Env[k], env[k])
		if src == "project config" {
			src += " (trust-gated)"
		}
		rows = append(rows, []string{"env." + k, env[k], src})
	}
	return rows
}
