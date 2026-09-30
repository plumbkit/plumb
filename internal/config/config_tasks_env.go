package config

import (
	"fmt"
	"regexp"
	"strings"
)

// config_tasks_env.go is [tasks.<lang>] env: environment variables set on every
// command of that language, so a stored command can run the way the project's CI
// runs it. The motivating case is plumb's own: CI runs `make test`, which sets
// GOTMPDIR to a directory inside the checkout, and a test whose outcome depends
// on where t.TempDir() lands passed under run_task and failed on CI (#537).
//
// The values are templates. {workspace} and {working_dir} expand where the
// command runs (internal/tools/task_env.go), not here, because mutation_test can
// move a command into another work-tree after it was resolved.
//
// Trust: an environment variable changes what a command runs as surely as the
// command does, so a project's entries are part of the trusted content (see
// taskSpecsFrom) and a project env makes every slot of that language
// project-supplied, the rule working_dir already follows.
//
// Concurrency: stateless; the regexps are read-only.

// TaskEnvKey is the [tasks.<lang>] key holding the environment table.
const TaskEnvKey = "env"

// The placeholders an env value may contain.
const (
	TaskEnvWorkspace  = "{workspace}"
	TaskEnvWorkingDir = "{working_dir}"
)

// taskEnvName is a POSIX-portable variable name. Anything else either cannot be
// passed through an environment block ('=', NUL) or is a typo nobody could have
// meant.
var taskEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// taskEnvPlaceholder finds every brace-delimited token in a value, so a
// misspelt placeholder is refused at load rather than handed to the command as
// literal text.
var taskEnvPlaceholder = regexp.MustCompile(`\{[^{}]*\}`)

// deniedTaskEnvKeys make the dynamic loader run extra code in EVERY process the
// command starts, the compiler and the shell included. No build or test needs
// them, trust notwithstanding, so they are refused outright rather than left to a
// reviewer reading the `plumb trust` listing. LD_LIBRARY_PATH and
// DYLD_LIBRARY_PATH are not here: native test dependencies legitimately need
// them, so they are disclosed with a warning instead (EnvKeySteersExecution).
var deniedTaskEnvKeys = map[string]bool{
	"LD_PRELOAD": true, "LD_AUDIT": true,
	"DYLD_INSERT_LIBRARIES": true, "DYLD_FORCE_FLAT_NAMESPACE": true,
}

// steeringEnvKeys change which program, or which code, a command runs. They are
// allowed — a toolchain on a custom PATH is ordinary — but `plumb trust` flags
// each one a project sets, so the consent is informed.
var steeringEnvKeys = map[string]bool{
	"PATH": true, "GOFLAGS": true, "GOTOOLCHAIN": true, "GOROOT": true,
	"NODE_OPTIONS": true, "PYTHONPATH": true, "PYTHONSTARTUP": true, "PYTHONHOME": true,
	"RUSTC_WRAPPER": true, "RUSTFLAGS": true, "RUSTC": true, "CARGO_BUILD_RUSTC_WRAPPER": true,
	"BASH_ENV": true, "ENV": true, "PERL5OPT": true, "RUBYOPT": true, "JAVA_TOOL_OPTIONS": true,
	"LD_LIBRARY_PATH": true,
}

// EnvKeySteersExecution reports whether an environment variable changes which
// program or code a command runs (see steeringEnvKeys), for the `plumb trust`
// disclosure.
func EnvKeySteersExecution(key string) bool {
	u := strings.ToUpper(key)
	return steeringEnvKeys[u] || strings.HasPrefix(u, "GIT_") || strings.HasPrefix(u, "DYLD_")
}

// TaskEnvKeyOf reports whether a TaskCommandSpec slot is an env entry, and the
// variable it names. taskSpecsFrom spells one as "<env key>.<name>"; a real slot
// can never contain a '.', so the two cannot be confused — and a project that
// tried to forge one with a quoted key fails to load (validateExtraTaskSlots), so
// it can never run anything under a hash it shares with a real env entry.
func TaskEnvKeyOf(slot string) (string, bool) {
	prefix, name, ok := strings.Cut(slot, ".")
	if !ok || !strings.EqualFold(prefix, TaskEnvKey) {
		return "", false
	}
	return name, true
}

// taskEnvSpecs flattens one [tasks.<lang>] env table into TaskCommandSpecs, so
// the entries are hashed and disclosed by `plumb trust` exactly like commands.
// The key spelling is kept, as taskSpecsFrom keeps every other.
func taskEnvSpecs(lang, key string, env map[string]any) []TaskCommandSpec {
	out := make([]TaskCommandSpec, 0, len(env))
	for name, v := range env {
		out = append(out, TaskCommandSpec{Lang: lang, Slot: key + "." + name, Command: fmt.Sprint(v)})
	}
	return out
}

// validateTaskEnv rejects a malformed or refused [tasks.<lang>] env entry: a name
// that is not a portable variable name, a denied loader variable, a NUL in the
// value, or a brace token that is not one of the two placeholders.
func validateTaskEnv(lang string, env map[string]string) error {
	for k, v := range env {
		switch {
		case !taskEnvName.MatchString(k):
			return fmt.Errorf("tasks.%s.env: %q is not a valid environment variable name "+
				"(a letter or _ first, then letters, digits or _)", lang, k)
		case deniedTaskEnvKeys[strings.ToUpper(k)]:
			return fmt.Errorf("tasks.%s.env.%s: refused — it makes the dynamic loader run extra code in every process the command starts, "+
				"and no build or test needs that", lang, k)
		case strings.ContainsRune(v, 0):
			return fmt.Errorf("tasks.%s.env.%s: an environment variable value must not contain a NUL byte", lang, k)
		}
		for _, p := range taskEnvPlaceholder.FindAllString(v, -1) {
			if p != TaskEnvWorkspace && p != TaskEnvWorkingDir {
				return fmt.Errorf("tasks.%s.env.%s: unknown placeholder %s (the placeholders are %s and %s)",
					lang, k, p, TaskEnvWorkspace, TaskEnvWorkingDir)
			}
		}
	}
	return nil
}

// composeTaskEnv applies composeGitEnv's rule to every language's env: the
// project's value wins for the names it sets, and a global entry it is silent
// about survives, whichever TOML spelling the project used.
func composeTaskEnv(base, merged map[string]TasksConfig) map[string]TasksConfig {
	for lang, tc := range merged {
		tc.Env = composeGitEnv(base[lang].Env, tc.Env)
		merged[lang] = tc
	}
	return merged
}
