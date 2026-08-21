package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// config_commands.go defines the safe command-execution config: the [[command]]
// allow-list (fixed-argv named commands the run_command tool may run) and the
// [commands] policy table (the execute_shell_command gate + sandbox policy).
//
// The allow-list is a capability table, not a template engine: an entry's exec
// is a fixed argv the runner execs WITHOUT a shell, so no agent free-text ever
// reaches the command line (the single {target} token is the one exception, and
// it is bounded to one shell-safe argument by the run_command tool). A project's
// .plumb/config.toml is an untrusted surface, so a project-supplied command — and
// a project raising [commands] allow_shell — is honoured only after `plumb trust`
// (gated at the cli resolution seam, mirroring [tasks]).
//
// Concurrency: CommandConfig / CommandsConfig values are read-only after Load.

// CommandConfig is one entry in the [[command]] allow-list.
type CommandConfig struct {
	// Name is the identifier run_command looks up. Unique within a config scope.
	Name string `toml:"name"`
	// Exec is the fixed argv, executed without a shell. A single element may be
	// the literal token "{target}", replaced at call time by run_command's
	// bounded target argument.
	Exec []string `toml:"exec"`
	// WorkingDir is the directory to run in, relative to the workspace root.
	// Empty or "." is the root. It must not escape the root (no absolute path,
	// no ".." segment).
	WorkingDir string `toml:"working_dir"`
	// Timeout bounds the command. 0 uses the runner default.
	Timeout Duration `toml:"timeout"`
	// AllowWrites lets the command write inside the workspace under the sandbox
	// (default: writes confined to $TMPDIR).
	AllowWrites bool `toml:"allow_writes"`
	// DenyNetwork cuts network access for the command under the sandbox
	// (default: network permitted, since build tools fetch dependencies).
	DenyNetwork bool `toml:"deny_network"`
}

// CommandsConfig is the [commands] policy table: the execute_shell_command gate
// and the sandbox-enforcement knob. Both are safety-sensitive, so a project's
// value is honoured only when the workspace is trusted (applied at the seam).
type CommandsConfig struct {
	// AllowShell gates the execute_shell_command tool. Default false (off). A
	// project raising it to true is honoured only after `plumb trust`.
	AllowShell bool `toml:"allow_shell"`
	// RequireSandbox, when true, refuses to run a command (either tool) when no
	// OS sandbox is active, rather than running unsandboxed with a warning.
	RequireSandbox bool `toml:"require_sandbox"`
	// DenyNetwork cuts network access for execute_shell_command. Default TRUE: the
	// sandbox is integrity-only (reads stay permissive and the command inherits the
	// daemon environment), so a shell command could otherwise read secrets and
	// exfiltrate over the network — so the network is off unless you opt in with
	// deny_network = false. It does not apply to run_command (each [[command]] sets
	// its own deny_network, default false).
	DenyNetwork bool `toml:"deny_network"`
}

// No [[command]] allow-list ships by default, and one nearly did.
//
// The motivation was real: [tasks] ships working per-language commands —
// including `go test ./...`, which runs arbitrary code out of the repository —
// while the [[command]] tier, fixed argv with no shell and no free text, shipped
// EMPTY and refused `wc -l`. The only documented way forward was then
// [commands] allow_shell, the broad read-everything tier, so callers enabled
// that or left plumb for their own shell — the outcome the safe tier exists to
// prevent.
//
// Three read-only entries (wc, file-type, disk-usage) were shipped against that
// reasoning and reverted, because run_command bounds {target} to one SHELL-safe
// argument, which is not a WORKSPACE-safe one. `^[A-Za-z0-9._/:@-]+$` admits
// `/`, `..`, `/etc/passwd` and `/dev/zero`, and nothing downstream confines a
// target to the workspace. Shipping them therefore handed every connected agent
// a file-type-and-existence oracle for any path the user can read, plus a
// ten-minute `du -sh /`, with no trust prompt and no action by the user. A
// user's own entry has always had that property — but that is their deliberate
// choice about their own machine, and shipping it is a different act.
//
// Ship defaults only with all three of:
//
//   - a per-entry target KIND (e.g. Target: "path") so run_command can tell a
//     path from an opaque argument;
//   - workspace containment for a path target, resolve-then-check so a symlink
//     cannot slip past a lexically-clean string (the #264 rule);
//   - an explicit Timeout — entries inherit the 10-minute task default, an
//     eternity for a metadata probe.
//
// And extend TestDefaults_CommandsAreReadOnlyAndBounded to cover the target's
// VALUE, not just the placeholder count: that gap is what let this through.

// TargetToken is the literal placeholder an exec argv may contain once; the
// run_command tool substitutes it with a bounded target argument.
const TargetToken = "{target}"

// TargetTokenPrefix opens the defaulted form, `{target:<default>}`, which stands
// in for the target when the caller gives none (an empty default omits the
// argument entirely). Substitution lives at the cli seam; this package only has
// to COUNT placeholders, and it has to count both spellings — an "at most once"
// rule that recognises one form lets `{target} {target:./...}` past, and the
// second substitution then lands in an argv nobody validated for it.
const TargetTokenPrefix = "{target:"

// IsTargetToken reports whether a whole argv element is a target placeholder in
// either spelling.
func IsTargetToken(arg string) bool {
	return arg == TargetToken ||
		(strings.HasPrefix(arg, TargetTokenPrefix) && strings.HasSuffix(arg, "}"))
}

// Find returns the command with the given name and whether it was found.
func FindCommand(cmds []CommandConfig, name string) (CommandConfig, bool) {
	for _, c := range cmds {
		if c.Name == name {
			return c, true
		}
	}
	return CommandConfig{}, false
}

// CommandNames returns the entry names in declaration order (for error messages
// and the tool's "available commands" hint).
func CommandNames(cmds []CommandConfig) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Name)
	}
	return out
}

// cloneCommands deep-copies the allow-list so a merged Config never shares the
// slice (or an entry's Exec) with another load.
func cloneCommands(cmds []CommandConfig) []CommandConfig {
	if cmds == nil {
		return nil
	}
	out := make([]CommandConfig, len(cmds))
	for i, c := range cmds {
		c.Exec = append([]string(nil), c.Exec...)
		out[i] = c
	}
	return out
}

// validateCommands rejects a malformed allow-list: a blank or duplicate name, an
// empty exec, a blank exec[0], more than one {target} token, a negative timeout,
// or a working_dir that escapes the workspace root. Note the exec argv is NOT
// rejected for shell metacharacters — it is exec'd directly (no shell), so a
// metacharacter is a literal argument, not shell syntax. A user who writes
// exec = ["sh","-c", ...] has deliberately opted into arbitrary execution; that
// irreducible risk is documented, not engineered away.
func validateCommands(cmds []CommandConfig) error {
	seen := make(map[string]bool, len(cmds))
	for i, c := range cmds {
		where := c.Name
		if where == "" {
			where = fmt.Sprintf("#%d", i+1)
		}
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("command %s: name must not be empty", where)
		}
		if seen[c.Name] {
			return fmt.Errorf("command %q: duplicate name in the same config scope", c.Name)
		}
		seen[c.Name] = true
		if len(c.Exec) == 0 {
			return fmt.Errorf("command %q: exec must be a non-empty argv", c.Name)
		}
		if strings.TrimSpace(c.Exec[0]) == "" {
			return fmt.Errorf("command %q: exec[0] (the program) must not be empty", c.Name)
		}
		if n := countTargetTokens(c.Exec); n > 1 {
			return fmt.Errorf("command %q: exec may contain the %s token at most once (found %d)", c.Name, TargetToken, n)
		}
		if c.Timeout.Duration < 0 {
			return fmt.Errorf("command %q: timeout must be non-negative (0 uses the default)", c.Name)
		}
		if err := validateCommandWorkingDir(c.WorkingDir); err != nil {
			return fmt.Errorf("command %q: %w", c.Name, err)
		}
	}
	return nil
}

// countTargetTokens counts exec elements that are a target placeholder, in
// either spelling.
func countTargetTokens(argv []string) int {
	n := 0
	for _, a := range argv {
		if IsTargetToken(a) {
			n++
		}
	}
	return n
}

// validateCommandWorkingDir rejects a working_dir that is absolute or escapes the
// workspace root via a ".." segment. Empty and "." mean the root.
func validateCommandWorkingDir(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	if filepath.IsAbs(dir) {
		return fmt.Errorf("working_dir %q must be relative to the workspace root", dir)
	}
	clean := filepath.Clean(dir)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("working_dir %q must not escape the workspace root", dir)
	}
	return nil
}
