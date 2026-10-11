package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
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
// each one a project sets, so the consent is informed, and a per-call run_task
// override may not touch one at all (ValidateTaskEnvOverrides).
//
// This is a GUARD RAIL, not a boundary, and it cannot be completed: make imports
// every variable of its environment as a make variable, so any name a Makefile
// reads with ?= or leaves undefined steers that build by name alone. What it does
// protect is the argv — the command line is still the one the project saved — and
// every name it catches is a name a project can still choose in [tasks.<lang>]
// env, where `plumb trust` shows it and a human consents to it.
var steeringEnvKeys = map[string]bool{
	"PATH": true, "GOFLAGS": true, "GOTOOLCHAIN": true, "GOROOT": true, "GOENV": true,
	"GOPROXY": true, "GOSUMDB": true, "GONOSUMDB": true, "GOINSECURE": true, "GOPRIVATE": true,
	"GOWORK": true, "GOMODCACHE": true, "GOPATH": true,
	"CC": true, "CXX": true, "HOME": true, "XDG_CONFIG_HOME": true,
	"NODE_OPTIONS": true, "NODE_PATH": true, "PYTHONPATH": true, "PYTHONSTARTUP": true, "PYTHONHOME": true,
	"PYTEST_ADDOPTS": true, "PYTEST_PLUGINS": true,
	"RUSTC_WRAPPER": true, "RUSTC_WORKSPACE_WRAPPER": true, "RUSTFLAGS": true, "RUSTC": true,
	"RUSTUP_TOOLCHAIN": true, "CARGO_BUILD_RUSTC_WRAPPER": true, "CARGO_HOME": true,
	"BASH_ENV": true, "ENV": true, "PERL5OPT": true, "PERL5LIB": true, "RUBYOPT": true, "RUBYLIB": true,
	"JAVA_TOOL_OPTIONS": true, "JDK_JAVA_OPTIONS": true, "_JAVA_OPTIONS": true, "LD_LIBRARY_PATH": true,
	// make reads its flags and its makefiles from the environment, ahead of
	// anything the Makefile itself sets.
	"MAKEFLAGS": true, "MAKEFILES": true,
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

// --- per-call overrides (run_task's env/unset, #634) --------------------------
//
// The [tasks.<lang>] env above is TRUSTED CONTENT: the project writes it, its
// entries are in the hash `plumb trust` records, and the ones that change which
// program or code runs are disclosed by name. A per-call override has no such
// gate — its caller is an agent, not the project — so it is held to two rules
// instead. Both are honest guard rails, not a sandbox, and neither is complete:
//
//   - A name that steers execution is refused, in BOTH directions. Setting
//     PATH, GOFLAGS=-toolexec=… or LD_PRELOAD replaces what the stored command
//     ends up executing; REMOVING a configured GOFLAGS=-mod=vendor or
//     GOWORK=off changes what it builds just as surely, and the state being
//     removed is the one the project chose and `plumb trust` approved — so "a
//     removal can only shrink the environment" was never the safety argument it
//     looked like. See steeringEnvKeys for what such a list cannot cover.
//   - Every other name is a plain value in the child's environment. Nothing
//     interprets it — the command is an argv, never a shell — so the hygiene
//     rule is the shape one: a portable name, one bounded line with no control
//     characters, and no unknown {…} token, which would otherwise reach the
//     command as literal text where the same typo in config is refused at load.
//
// What run_task promises is the ARGV: the stored command, with no shell and no
// agent-supplied command line. The environment is guarded, not sealed.

// maxTaskEnvOverride is the longest per-call env value. The cap is hygiene
// against a malformed client, not a limit any build or test comes near.
const maxTaskEnvOverride = 4096

// maxTaskEnvOverrides bounds how many names ONE call may change, in both
// directions together, so a client cannot build an unbounded environment block.
const maxTaskEnvOverrides = 64

// ValidateTaskEnvOverrides refuses a run_task call's per-call env changes as a
// whole: every name, every value, the two lists against each other, and the size
// of the input. nil means the call may run.
//
// The resolver at the cli seam calls this, not the tool, because the taxonomy it
// applies (deniedTaskEnvKeys, EnvKeySteersExecution) lives in this package and
// internal/tools deliberately does not import config — the resolver is the one
// layer that sees both. One rule with one owner: an agent env name half-checked
// in the tool and half here is how the two would drift.
//
// Refusals are ordered deterministically (sorted names) so a call that is wrong
// in several ways is refused for the same reason every time.
func ValidateTaskEnvOverrides(set map[string]string, unset []string) error {
	if len(set)+len(unset) > maxTaskEnvOverrides {
		return fmt.Errorf("run_task: %d environment changes in one call is more than the %d allowed",
			len(set)+len(unset), maxTaskEnvOverrides)
	}
	for _, name := range slices.Sorted(slices.Values(unset)) {
		if err := validateTaskEnvOverrideName(name, "unset"); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(set)) {
		if err := validateTaskEnvOverrideName(name, TaskEnvKey); err != nil {
			return err
		}
		if err := validateTaskEnvOverrideValue(name, set[name]); err != nil {
			return err
		}
	}
	// Last, so a caller who spelled the same variable in both lists hears about
	// the variable itself first.
	for _, name := range slices.Sorted(maps.Keys(set)) {
		if slices.Contains(unset, name) {
			return fmt.Errorf("run_task: %s is named in both env and unset; one call cannot set a variable and remove it", name)
		}
	}
	return nil
}

// validateTaskEnvOverrideName applies the one name rule both directions share:
// portable, and not a name that decides what the command runs. list is the
// argument the caller used, so the refusal names the list they wrote.
//
// Removal is held to the same rule as setting, which is not obvious and was the
// other way round in the first draft of this file. Taking a configured
// `GOWORK=off` or `GOFLAGS=-mod=vendor` back out changes what the command builds
// as surely as putting one in does, and what it removes is the state the project
// chose and `plumb trust` approved.
func validateTaskEnvOverrideName(name, list string) error {
	switch {
	case !taskEnvName.MatchString(name):
		return fmt.Errorf("run_task: %s %q is not a valid environment variable name "+
			"(a letter or _ first, then letters, digits or _)", list, name)
	case deniedTaskEnvKeys[strings.ToUpper(name)]:
		return fmt.Errorf("run_task: %s %s is refused — a dynamic-loader variable makes every process "+
			"the command starts run extra code, and no build or test needs that", list, name)
	case EnvKeySteersExecution(name):
		if list == "unset" {
			return fmt.Errorf("run_task: unset %s is refused — removing it changes what the stored "+
				"command would run (a configured value, or one it inherits). A project that wants it "+
				"gone changes [tasks.<lang>] env, where `plumb trust` shows it", name)
		}
		return fmt.Errorf("run_task: env %s is refused for a single call — it changes which program, "+
			"or which code, the stored command runs. A project that needs it sets it in "+
			"[tasks.<lang>] env, where `plumb trust` lists it and flags it", name)
	}
	return nil
}

// validateTaskEnvOverrideValue holds a per-call value to the shape a caller can
// predict: one bounded line, and no brace token that is not one of the two
// placeholders — the rule a config entry is refused at load for, so the same
// typo does not reach the command as literal text here.
func validateTaskEnvOverrideValue(name, value string) error {
	control := func(r rune) bool { return r < 0x20 || r == 0x7f }
	switch {
	case strings.ContainsRune(value, 0):
		return fmt.Errorf("run_task: env %s: a value must not contain a NUL byte", name)
	case strings.ContainsFunc(value, control):
		return fmt.Errorf("run_task: env %s: a value must be one line with no control characters", name)
	case len(value) > maxTaskEnvOverride:
		return fmt.Errorf("run_task: env %s: %d bytes is longer than the %d a per-call value may be",
			name, len(value), maxTaskEnvOverride)
	}
	for _, p := range taskEnvPlaceholder.FindAllString(value, -1) {
		if p != TaskEnvWorkspace && p != TaskEnvWorkingDir {
			return fmt.Errorf("run_task: env %s: unknown placeholder %s (the placeholders are %s and %s)",
				name, p, TaskEnvWorkspace, TaskEnvWorkingDir)
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
