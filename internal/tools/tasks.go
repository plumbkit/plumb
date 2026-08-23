package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// tasks.go is the run_task MCP tool: it executes a STORED per-language command
// (build/lint/test/e2e/verify, or a project-defined slot) resolved by the daemon, never an agent-supplied
// command line. The only agent input that reaches the argv is an optional
// {target} token, shell-escaped by validation. Resolution + the per-workspace
// trust gate live in the daemon (the resolver closure); this file is the MCP
// surface and the bounded execution. No config import — the resolver bridges it.
//
// Concurrency: Execute is safe for concurrent use (no shared mutable state).

// taskSlotName bounds the slot argument to a plain lowercase identifier. It is
// INPUT HYGIENE, not the vocabulary: which slots exist is the config layer's
// answer, and this file deliberately does not import it (see the file comment)
// — the resolver bridges it. Keeping a closed set here is what made the slot
// vocabulary Go-shaped: a project whose toolchain calls its verb `check` could
// not reach run_task at all, and fell back to raw shell, losing the no-shell
// argv contract and the trust gate with it.
//
// TestTaskSlotNamePattern_MatchesConfig pins this against
// config.ValidTaskSlotName so the two cannot drift.
var taskSlotName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// targetPattern bounds the {target} token to a single shell-safe argument.
var targetPattern = regexp.MustCompile(`^[A-Za-z0-9._/:@-]+$`)

// TaskCommand is a resolved, ready-to-run task: one or more argv steps run in
// sequence (verify is build then test), with the config layer it came from.
type TaskCommand struct {
	Slot       string
	Steps      [][]string // one argv per step; empty ⇒ nothing to run
	Provenance string     // "default" | "global" | "project"
	// WorkingDir is the absolute directory to run in, already resolved and
	// boundary-checked by the resolver. Empty falls back to the workspace root,
	// which is what every caller got before [tasks.<lang>] working_dir existed.
	WorkingDir string
	// Language and Configured describe the resolution CONTEXT, and are set even
	// when Steps is empty — that is the case they exist for. "no test command
	// configured for this workspace" named neither the language it resolved for
	// nor which slots do have commands, so an agent that hit it had no way to tell
	// an unconfigured slot from an unconfigured workspace, and fell back to raw
	// shell for every build and test.
	Language   string
	Configured []string // slots that DO have a command, for the empty-slot message
	// ConfigPath is the absolute path of the project config file a command for
	// this slot would be written to. It is the difference between a remedy a
	// caller can act on and one it has to go looking for: ".plumb/config.toml" is
	// relative to a workspace root the agent may not have in hand, and an agent
	// that cannot find the file falls back to raw shell instead. Empty when the
	// resolver could not name one, in which case the relative form is used.
	ConfigPath string
}

// noCommandError explains an unconfigured slot in terms the caller can act on:
// which language was resolved, what is configured for it, and how to fix it.
func noCommandError(cmd TaskCommand, slot string) error {
	have := "no slots are configured for it"
	if len(cmd.Configured) > 0 {
		have = "configured slots: " + strings.Join(cmd.Configured, ", ")
	}
	// The subject and the config KEY are separate strings on purpose. Reusing one
	// variable for both produced `[tasks.this workspace]` when the language was
	// unknown — not valid TOML, and not actionable, in the one case where the
	// caller most needs telling what to do.
	subject, key := cmd.Language, cmd.Language
	if subject == "" {
		subject, key = "this workspace", "<lang>"
	}
	where := ".plumb/config.toml"
	if cmd.ConfigPath != "" {
		where = cmd.ConfigPath
	}
	return fmt.Errorf(
		"run_task: no %s command configured for %s (%s). "+
			"Set one with [tasks.%s] %s = \"...\" in %s, "+
			"or via agent_config op=set when the user has enabled [agent_config_writes]",
		slot, subject, have, key, slot, where)
}

// TaskResolverFn resolves a slot (+ optional target) to a runnable command for
// the session's workspace, applying the per-workspace trust gate. It returns an
// error when the slot has no command, or when a project-supplied command is not
// yet trusted. nil ⇒ the tool reports task commands are unavailable.
type TaskResolverFn = func(slot, target string) (TaskCommand, error)

// Tasks is the run_task MCP tool.
type Tasks struct {
	deps    WriteDeps
	resolve TaskResolverFn
}

func NewTasks(deps WriteDeps, resolve TaskResolverFn) *Tasks {
	return &Tasks{deps: deps, resolve: resolve}
}

var runTaskSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "slot": {
      "type": "string",
      "description": "Which stored task command to run: build, lint, test, e2e, verify, or a project-defined slot under [tasks.<lang>]. session_start lists what's configured; an unconfigured slot is refused with that list."
    },
    "target": {
      "type": "string",
      "description": "Optional target substituted for a {target} token in the stored command (e.g. a single test name or package). The shipped go/python/rust test defaults carry a defaulted placeholder ({target:./...}), so scoping works with no config edit and omitting the target still runs everything. Restricted to one shell-safe argument ([A-Za-z0-9._/:@-]); refused if the command has no {target} slot."
    }
  },
  "required": ["slot"],
  "additionalProperties": false
}`)

func (t *Tasks) Name() string                 { return "run_task" }
func (t *Tasks) InputSchema() json.RawMessage { return runTaskSchema }
func (t *Tasks) Description() string {
	return "Run a stored per-language task command — build, lint, test, e2e, verify, or a project-defined slot — configured in [tasks.<lang>]. " +
		"It executes only the command the user saved for this workspace's language (no shell, no agent-supplied command line); the optional target fills a {target} placeholder with one shell-safe argument, and the shipped test defaults carry one so scoping needs no config edit. " +
		"Commands run from the workspace root, or from [tasks.<lang>] working_dir when the module lives in a subdirectory. " +
		"A project-supplied (.plumb/config.toml) command must be trusted first (run `plumb trust`); the shipped defaults and global-config commands always run. Output and runtime are bounded. " +
		"Pairs with topology_affected (which says WHICH tests to run; this runs them)."
}

type runTaskArgs struct {
	Slot   string `json:"slot"`
	Target string `json:"target"`
}

func (a runTaskArgs) validate() error {
	if !taskSlotName.MatchString(a.Slot) {
		return fmt.Errorf("run_task: slot %q is not a valid slot name "+
			"(lowercase letter first, then letters, digits, _ or -, max 32 characters); "+
			"the built-ins are build, lint, test, e2e, verify", a.Slot)
	}
	if a.Target != "" && !targetPattern.MatchString(a.Target) {
		return fmt.Errorf("run_task: target %q is not a single shell-safe argument ([A-Za-z0-9._/:@-])", a.Target)
	}
	return nil
}

func (t *Tasks) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a runTaskArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("run_task: invalid arguments: %w", err)
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	if t.resolve == nil {
		return "", errors.New("run_task: task commands are not available for this session")
	}
	cmd, err := t.resolve(a.Slot, a.Target)
	if err != nil {
		return "", err
	}
	if len(cmd.Steps) == 0 {
		return "", noCommandError(cmd, a.Slot)
	}
	return t.run(ctx, cmd)
}

func (t *Tasks) workspace(ctx context.Context) string {
	if t.deps.WorkspaceFn == nil {
		return ""
	}
	return t.deps.WorkspaceFn(ctx)
}

// run executes each step in sequence, stopping at the first non-zero exit, and
// renders a compact report.
func (t *Tasks) run(ctx context.Context, cmd TaskCommand) (string, error) {
	ws := cmd.WorkingDir
	if ws == "" {
		ws = t.workspace(ctx)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "run_task %s (source=%s)\n", cmd.Slot, cmd.Provenance)
	for i, argv := range cmd.Steps {
		res, err := RunArgv(ctx, ws, argv, defaultTaskTimeout)
		if err != nil {
			return "", fmt.Errorf("run_task %s: %w", cmd.Slot, err)
		}
		b.WriteString(formatStep(argv, res))
		if res.ExitCode != 0 {
			fmt.Fprintf(&b, "→ stopped: step %d/%d failed (exit %d)\n", i+1, len(cmd.Steps), res.ExitCode)
			return b.String(), nil
		}
	}
	b.WriteString("→ ok\n")
	return b.String(), nil
}

// formatStep renders one step's command line, exit status, and bounded output.
func formatStep(argv []string, res ExecResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", strings.Join(argv, " "))
	if res.TimedOut {
		b.WriteString("(timed out)\n")
	}
	if out := strings.TrimSpace(res.Stdout); out != "" {
		b.WriteString(out)
		b.WriteString("\n")
	}
	if errOut := strings.TrimSpace(res.Stderr); errOut != "" {
		b.WriteString(errOut)
		b.WriteString("\n")
	}
	return b.String()
}
