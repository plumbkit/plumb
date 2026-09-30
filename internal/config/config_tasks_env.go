package config

import (
	"fmt"
	"path/filepath"
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
// project-supplied, the rule working_dir already follows. One exception
// (TaskEnvIsScratchDir): GOTMPDIR pointing inside the workspace
// changes neither what runs nor where, so it alone does not make the shipped
// defaults need trust, and a checked-in `GOTMPDIR = "{workspace}/.testcache"`
// does not break run_task in every fresh clone and worktree.
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
	"PATH": true, "GOFLAGS": true, "GOTOOLCHAIN": true, "GOROOT": true, "GOENV": true,
	"GOPROXY": true, "GOSUMDB": true, "GONOSUMDB": true, "GOINSECURE": true, "GOPRIVATE": true,
	"CC": true, "CXX": true, "HOME": true, "XDG_CONFIG_HOME": true,
	"NODE_OPTIONS": true, "NODE_PATH": true, "PYTHONPATH": true, "PYTHONSTARTUP": true, "PYTHONHOME": true,
	"RUSTC_WRAPPER": true, "RUSTC_WORKSPACE_WRAPPER": true, "RUSTFLAGS": true, "RUSTC": true,
	"CARGO_BUILD_RUSTC_WRAPPER": true, "CARGO_HOME": true,
	"BASH_ENV": true, "ENV": true, "PERL5OPT": true, "PERL5LIB": true, "RUBYOPT": true, "RUBYLIB": true,
	"JAVA_TOOL_OPTIONS": true, "LD_LIBRARY_PATH": true,
}

// steeringEnvPrefixes are families of variables that steer execution the same way:
// git's (hooks, ssh command, config), the macOS loader's, cgo's compiler flags,
// cargo's per-target runner and linker, and npm's config.
var steeringEnvPrefixes = []string{"GIT_", "DYLD_", "CGO_", "CARGO_TARGET_", "NPM_CONFIG_"}

// scratchDirEnvKeys name a directory a command writes temporary files to and
// nothing else; see TaskEnvIsScratchDir. Only GOTMPDIR, the one a checked-in
// config needs to mirror CI (#537): least privilege, since TMPDIR reaches every
// tool of every language, not only go's build and test temp files.
var scratchDirEnvKeys = map[string]bool{"GOTMPDIR": true}

// TaskEnvIsScratchDir reports whether a [tasks.<lang>] env entry only moves
// temporary files to a directory inside the workspace: a scratch-directory key
// whose value is {workspace} or {workspace}/<a relative path that stays inside>.
// Such an entry changes neither what runs nor where it runs — the shipped default
// already runs the workspace's own code — so it is exempt from the rule that a
// project env makes every slot need trust. It is still hashed like every entry.
func TaskEnvIsScratchDir(key, value string) bool {
	if !scratchDirEnvKeys[strings.ToUpper(key)] {
		return false
	}
	if value == TaskEnvWorkspace {
		return true
	}
	rest, ok := strings.CutPrefix(value, TaskEnvWorkspace+"/")
	return ok && !strings.ContainsAny(rest, "{}\\") && filepath.IsLocal(rest)
}

// EnvKeySteersExecution reports whether an environment variable changes which
// program or code a command runs (see steeringEnvKeys), for the `plumb trust`
// disclosure.
func EnvKeySteersExecution(key string) bool {
	u := strings.ToUpper(key)
	if steeringEnvKeys[u] {
		return true
	}
	for _, p := range steeringEnvPrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
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
