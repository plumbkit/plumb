package config

import (
	"fmt"
	"maps"
	"strings"
)

// config_tasks.go defines the [tasks.<lang>] config section: per-language
// build/lint/test/e2e/verify command templates, plus the safe defaults plumb
// ships. A command is a single argv executed without a shell (see the task
// runner); the verify slot is a composite that runs build then test in
// sequence, so it carries no executable string of its own.
//
// Concurrency: TasksConfig values are read-only after Load returns.

// TasksConfig holds the five command slots for one language. An empty slot
// means "no command for this language" — never a guessed tool that may be
// absent. The {target} placeholder is honoured only in the Test slot.
type TasksConfig struct {
	Build  string `toml:"build"`
	Lint   string `toml:"lint"`
	Test   string `toml:"test"`
	E2E    string `toml:"e2e"`
	Verify string `toml:"verify"` // composite: runs Build then Test; stores no command
}

// TaskSlots are the valid slot names, in display order.
var TaskSlots = []string{"build", "lint", "test", "e2e", "verify"}

// Get returns the command stored in the named slot ("" for verify, which is a
// composite). An unknown slot returns "".
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
	default:
		return ""
	}
}

// defaultTasks returns the shipped per-language command defaults. Where a tool
// is not part of a language's standard toolchain (and may not be installed) the
// slot is left empty rather than guessed. The verify slot is always empty: it
// is a composite of build then test, handled by the runner.
func defaultTasks() map[string]TasksConfig {
	return map[string]TasksConfig{
		"go": {
			Build: "go build ./...",
			Lint:  "golangci-lint run",
			Test:  "go test ./...",
			E2E:   "go test -tags=integration ./...",
		},
		"python": {
			Test: "pytest",
			Lint: "ruff check .",
		},
		"rust": {
			Build: "cargo build",
			Lint:  "cargo clippy",
			Test:  "cargo test",
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
		return nil, fmt.Errorf("task command is empty after trimming")
	}
	return argv, nil
}

// cloneTasks deep-copies a tasks map so a merged Config never shares the map
// with another load.
func cloneTasks(m map[string]TasksConfig) map[string]TasksConfig {
	if m == nil {
		return nil
	}
	out := make(map[string]TasksConfig, len(m))
	maps.Copy(out, m)
	return out
}
