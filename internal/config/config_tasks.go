package config

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// config_tasks.go defines the [tasks.<lang>] config section: per-language
// build/lint/test/e2e/verify command templates, plus the safe defaults plumb
// ships. A command is a single argv executed without a shell (see the task
// runner); the verify slot is a composite that runs build then test in
// sequence, so it carries no executable string of its own.
//
// Concurrency: TasksConfig values are read-only after Load returns.

// TasksConfig holds the command slots for one language: the five built-ins
// below, plus any extra slot the project names for itself (Extra). An empty
// slot means "no command for this language" — never a guessed tool that may be
// absent. The {target} placeholder is honoured only in the Test slot.
type TasksConfig struct {
	Build  string `toml:"build"`
	Lint   string `toml:"lint"`
	Test   string `toml:"test"`
	E2E    string `toml:"e2e"`
	Verify string `toml:"verify"` // composite: runs Build then Test; stores no command
	// WorkingDir is the directory these commands run in, relative to the
	// workspace root. Empty or "." is the root — the historical behaviour and
	// still the default.
	//
	// It exists because the workspace root is not always the module root. A
	// holder repository — no go.mod at the top, only a go.work pointing at a
	// subdirectory — made EVERY Go task command fail instantly (`go build ./...`
	// exits 1 with "directory prefix . does not contain modules listed in
	// go.work"), and took mutation_test with it, since its compile gate could
	// never pass. Naming the module directory here is the fix.
	//
	// Explicit rather than inferred: a workspace may hold several modules, and
	// silently relocating a command the user already has working is worse than
	// asking. Validated relative and non-escaping at load
	// (validateCommandWorkingDir), and re-checked against the workspace boundary
	// after symlink resolution when it is resolved to an absolute path.
	WorkingDir string `toml:"working_dir"`
	// Extra holds slots this config named that are not one of the five built-ins
	// — `check`, `typecheck`, `audit`, whatever the project's toolchain calls
	// its verbs. The built-in five are Go-shaped, and a project whose verb is not
	// among them previously had no way to reach run_task at all: the slot
	// vocabulary was closed, so `pnpm check` had to be run through raw shell,
	// losing the no-shell argv contract and the trust gate with it.
	//
	// Decoded separately from this struct (extraTaskSlots), because go-toml
	// binds only the declared fields. It is NOT a toml-tagged field: an unknown
	// key must not round-trip through it on save, or a typo would be preserved
	// as a slot forever.
	//
	// Extras are trust-gated exactly like a built-in — ProjectTaskCommands
	// already flattens every (lang, slot, command) triple regardless of slot
	// name — and are NOT agent-writable, since agentWritableKeys is keyed by
	// registry field and an extra has no registry entry (fail closed).
	Extra map[string]string `toml:"-"`
}

// TaskSlots are the built-in slot names, in display order. A project may name
// further slots of its own (see TasksConfig.Extra); this list stays the
// built-in set, because it is what "not configured" is reported against — an
// extra slot is opt-in and is never missing.
var TaskSlots = []string{"build", "lint", "test", "e2e", "verify"}

// IsBuiltinTaskSlot reports whether name is one of the five built-in slots.
func IsBuiltinTaskSlot(name string) bool {
	return slices.Contains(TaskSlots, name)
}

// taskSlotName bounds an extra slot's NAME (its command is bounded separately,
// by ParseTaskCommand). A slot name reaches an error message and a TOML key, and
// is matched against agent input, so it is kept to a plain lowercase identifier
// rather than left to whatever a config file happens to contain.
var taskSlotName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ValidTaskSlotName reports whether name is well formed as a slot name. It does
// not say the slot exists — an unconfigured-but-well-formed name is resolved,
// then reported by run_task with the list of slots that DO have a command.
func ValidTaskSlotName(name string) bool {
	return taskSlotName.MatchString(name)
}

// Get returns the command stored in the named slot ("" for verify, which is a
// composite). A name that is not a built-in falls through to the project's own
// extra slots; an unknown slot returns "".
func (t TasksConfig) Get(slot string) string {
	switch slot {
	case "build":
		return t.Build
	case "lint":
		return t.Lint
	case "test":
		return t.Test
	case "e2e":
		return t.E2E
	case "verify":
		return "" // composite: the runner builds it from build + test
	default:
		return t.Extra[slot]
	}
}

// ConfiguredSlotNames returns every slot name this config could run, built-ins
// first in canonical order and the project's own extras after, sorted. It names
// what EXISTS, not what has a command — callers that need the latter (session
// orientation, run_task's "configured slots" message) filter it through
// buildTaskSteps, which is the single source of truth on runnability.
func ConfiguredSlotNames(t TasksConfig) []string {
	out := make([]string, 0, len(TaskSlots)+len(t.Extra))
	out = append(out, TaskSlots...)
	extras := make([]string, 0, len(t.Extra))
	for name := range t.Extra {
		extras = append(extras, name)
	}
	sort.Strings(extras)
	return append(out, extras...)
}

// defaultTasks returns the shipped per-language command defaults. Where a tool
// is not part of a language's standard toolchain (and may not be installed) the
// slot is left empty rather than guessed. The verify slot is always empty: it
// is a composite of build then test, handled by the runner.
//
// The test slots carry a DEFAULTED placeholder, `{target:<all>}`, so scoping
// works on an unmodified install while a bare run_task still runs everything.
// Without it neither run_task's target nor mutation_test's test_target could be
// used at all — substitution refuses a target the command has no slot for — so a
// mutation run paid for the whole suite per mutant, with no way to say otherwise.
//
// Only languages whose runner takes a POSITIONAL scope get one. go and python
// take a package path or test path; rust's `cargo test <filter>` takes a name
// substring, so its default is empty (the argument is dropped when no target is
// given, since "everything" there is the absence of the argument). typescript,
// swift and zig scope through flags whose spelling depends on the project's
// runner, and a guess that is wrong is worse than no placeholder — those keep
// their commands unchanged.
func defaultTasks() map[string]TasksConfig {
	return map[string]TasksConfig{
		"go": {
			Build: "go build ./...",
			Lint:  "golangci-lint run",
			Test:  "go test {target:./...}",
			E2E:   "go test -tags=integration ./...",
		},
		"python": {
			Test: "pytest {target:}",
			Lint: "ruff check .",
		},
		"rust": {
			Build: "cargo build",
			Lint:  "cargo clippy",
			Test:  "cargo test {target:}",
		},
		"typescript": {
			Build: "npm run build",
			Test:  "npm test",
		},
		"swift": {
			Build: "swift build",
			Test:  "swift test",
		},
		"zig": {
			Build: "zig build",
			Test:  "zig build test",
		},
	}
}

// builtinTaskKeys are the keys go-toml binds to a declared TasksConfig field.
// Everything else under [tasks.<lang>] is an extra slot.
var builtinTaskKeys = map[string]bool{
	"build": true, "lint": true, "test": true, "e2e": true, "verify": true,
	"working_dir": true,
}

// extraTaskSlots reads the project-named slots out of an already-decoded raw
// TOML document, keyed by language. It is a SECOND decode of the same bytes,
// the pattern LoadProjectWithPolicy already uses, rather than a custom
// UnmarshalTOML on TasksConfig: go-toml v2 hands an UnmarshalTOML raw bytes and
// takes over the whole decode for that value, so it would mean hand-parsing the
// table and reimplementing the layered merge — the same hand-rolled-parsing
// class as the gate bypass ProjectTaskCommands documents.
//
// Lookup is case-insensitive (rawTables), for exactly that reason: go-toml binds
// a table name to a struct field case-insensitively, so `[TASKS.go]` reaches the
// runner. An exact raw["tasks"] lookup here would miss it — the extra would be
// invisible to validation while remaining runnable, which is the shape of the
// bug the exact lookup already caused once.
func extraTaskSlots(raw map[string]any) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, tasks := range rawTables(raw, "tasks") {
		for lang, v := range tasks {
			slots, ok := v.(map[string]any)
			if !ok {
				continue
			}
			for slot, cv := range slots {
				if builtinTaskKeys[strings.ToLower(slot)] {
					continue
				}
				cmd, ok := cv.(string)
				if !ok {
					// A non-string under a slot name is not silently dropped: it is
					// recorded as an empty command so validateTasks sees the slot and
					// can reject the name if it is malformed. Dropping it here would
					// hide a config error rather than report it.
					cmd = ""
				}
				lower := strings.ToLower(lang)
				if out[lower] == nil {
					out[lower] = map[string]string{}
				}
				out[lower][slot] = cmd
			}
		}
	}
	return out
}

// applyExtraTaskSlots merges decoded extras onto cfg.Tasks. Later layers win per
// slot, matching how go-toml merges the declared fields, so a project may
// override a global extra without erasing the rest.
func applyExtraTaskSlots(tasks map[string]TasksConfig, extras map[string]map[string]string) map[string]TasksConfig {
	if len(extras) == 0 {
		return tasks
	}
	if tasks == nil {
		tasks = map[string]TasksConfig{}
	}
	for lang, slots := range extras {
		tc := tasks[lang]
		if tc.Extra == nil {
			tc.Extra = map[string]string{}
		} else {
			tc.Extra = maps.Clone(tc.Extra)
		}
		maps.Copy(tc.Extra, slots)
		tasks[lang] = tc
	}
	return tasks
}

// taskShellMetachars are sequences that imply shell interpretation. The runner
// execs an argv directly, so a command containing one would not behave as the
// author intends — reject it rather than silently mis-run it.
var taskShellMetachars = []string{"&&", "||", ";", "|", "$(", "`", ">", "<", "\n", "&"}

// ParseTaskCommand splits a task command string into an argv, enforcing the
// no-shell contract. An empty string yields a nil argv (an unset slot, not an
// error). A string containing a shell control metacharacter is rejected.
// Quoting is not interpreted — arguments are whitespace-separated.
func ParseTaskCommand(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	for _, m := range taskShellMetachars {
		if strings.Contains(s, m) {
			return nil, fmt.Errorf("task command may not contain shell metacharacter %q (commands run without a shell)", m)
		}
	}
	argv := strings.Fields(s)
	if len(argv) == 0 {
		return nil, errors.New("task command is empty after trimming")
	}
	return argv, nil
}

// ProjectTaskCommands enumerates the task commands a workspace's project config
// (<workspace>/.plumb/config.toml) explicitly supplies — every (lang, slot,
// command) it overrides, and only those (default- and global-config commands
// need no trust and are not included). It reads the raw project TOML so the set
// is provenance-filtered by construction and independent of which language is
// currently detected. This is the command set the trust hash binds to (see
// TrustStore.SetTrustedForProject / IsTrustedForTasks).
func ProjectTaskCommands(root string) ([]TaskCommandSpec, error) {
	raw, err := LoadProjectRaw(root)
	if err != nil {
		return nil, err
	}
	// rawTables, not raw["tasks"]. An exact-name lookup here is a GATE BYPASS:
	// go-toml/v2 binds a table name to a struct field case-insensitively, so
	// `[TASKS.go]` decodes into Config.Tasks and its command reaches the runner,
	// while `raw["tasks"]` misses it entirely — the command is absent from this
	// set, therefore absent from the trust hash, and taskProvenance (which used
	// the same exact lookup) reported it as not-project-supplied, so the gate was
	// skipped altogether. A cloned repository shipping `[TASKS.go] test = ...`
	// could have it run by run_task with no `plumb trust`. Same defect class as
	// #243's `Command`, in the sibling that was not audited with it.
	var out []TaskCommandSpec
	for _, tasks := range rawTables(raw, "tasks") {
		out = append(out, taskSpecsFrom(tasks)...)
	}
	return out, nil
}

// taskSpecsFrom flattens one [tasks] table into (lang, slot, command) triples.
// Keys keep the spelling the project used, so two spellings of one language both
// appear and both are hashed — the same rule projectPolicySpecFrom follows.
func taskSpecsFrom(tasks map[string]any) []TaskCommandSpec {
	var out []TaskCommandSpec
	for lang, v := range tasks {
		slots, ok := v.(map[string]any)
		if !ok {
			continue
		}
		for slot, cv := range slots {
			cmd, ok := cv.(string)
			if !ok {
				continue
			}
			out = append(out, TaskCommandSpec{Lang: lang, Slot: slot, Command: cmd})
		}
	}
	return out
}

// inlineInterpreters is the set of argv[0] basenames that execute code passed
// inline (via an inlineCodeFlags flag) rather than from a file. A command whose
// argv[0] is one of these AND carries an inline-code flag is arbitrary code
// execution by design — see FlagsInlineInterpreter.
var inlineInterpreters = map[string]bool{
	"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true,
	"python": true, "python2": true, "python3": true,
	"node": true, "nodejs": true, "deno": true,
	"perl": true, "ruby": true,
}

// inlineCodeFlags are the flags that make an interpreter run its argument as
// code: `-c` (POSIX shells, python), `-e`/`-E` (perl, ruby, node), `--eval`
// (node, deno), `--command`.
var inlineCodeFlags = map[string]bool{
	"-c": true, "-e": true, "-E": true, "--eval": true, "--command": true,
}

// FlagsInlineInterpreter reports whether argv invokes a known interpreter with an
// inline-code flag (e.g. `bash -c '…'`, `python -c '…'`, `node -e '…'`,
// `perl -e '…'`, `ruby -e '…'`) — arbitrary code execution by design, which the
// no-shell argv contract and the shell-metacharacter denylist do not catch
// (argv[0] is the interpreter and the code rides in a single quoted argument).
// It is defence-in-depth signal, not a hard reject: a user may legitimately run
// such a command from their own global config. `plumb trust` uses it to warn on
// each project-supplied command matching the pattern so consent is informed.
// `bash script.sh` and `python script.py` (a file, no inline flag) do not match.
func FlagsInlineInterpreter(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	if !inlineInterpreters[filepath.Base(argv[0])] {
		return false
	}
	for _, a := range argv[1:] {
		if inlineCodeFlags[a] {
			return true
		}
	}
	return false
}

// cloneTasks deep-copies a tasks map so a merged Config never shares the map
// with another load.
func cloneTasks(m map[string]TasksConfig) map[string]TasksConfig {
	if m == nil {
		return nil
	}
	out := make(map[string]TasksConfig, len(m))
	maps.Copy(out, m)
	// maps.Copy is shallow, and TasksConfig now carries a map of its own — so
	// without this the Extra maps stay SHARED with the config being cloned from,
	// and a project layer adding a slot would reach back into the global config
	// every later load starts from.
	for lang, tc := range out {
		if tc.Extra != nil {
			tc.Extra = maps.Clone(tc.Extra)
			out[lang] = tc
		}
	}
	return out
}
