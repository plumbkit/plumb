package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// tasks.go is the run_task MCP tool: it executes a STORED per-language command
// (build/lint/test/e2e/verify) resolved by the daemon, never an agent-supplied
// command line. The only agent input that reaches the argv is an optional
// {target} token, shell-escaped by validation. Resolution + the per-workspace
// trust gate live in the daemon (the resolver closure); this file is the MCP
// surface and the bounded execution. No config import — the resolver bridges it.
//
// Concurrency: Execute is safe for concurrent use (no shared mutable state).

// taskSlots are the runnable slot names.
var taskSlots = map[string]bool{"build": true, "lint": true, "test": true, "e2e": true, "verify": true}

// targetPattern bounds the {target} token to a single shell-safe argument.
var targetPattern = regexp.MustCompile(`^[A-Za-z0-9._/:@-]+$`)

// TaskCommand is a resolved, ready-to-run task: one or more argv steps run in
// sequence (verify is build then test), with the config layer it came from.
type TaskCommand struct {
	Slot       string
	Steps      [][]string // one argv per step; empty ⇒ nothing to run
	Provenance string     // "default" | "global" | "project"
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
      "enum": ["build", "lint", "test", "e2e", "verify"],
      "description": "Which stored task command to run: build, lint, test, e2e (integration), or verify (build then test). The command is configured per language in [tasks.<lang>] and resolved for this workspace's language — you cannot pass an arbitrary command."
    },
    "target": {
      "type": "string",
      "description": "Optional target substituted for a literal {target} token in the stored command (e.g. a single test name or package). Restricted to one shell-safe argument ([A-Za-z0-9._/:@-]); refused if the stored command has no {target}."
    }
  },
  "required": ["slot"],
  "additionalProperties": false
}`)

func (t *Tasks) Name() string                 { return "run_task" }
func (t *Tasks) InputSchema() json.RawMessage { return runTaskSchema }
func (t *Tasks) Description() string {
	return "Run a stored per-language task command — build, lint, test, e2e, or verify (build then test) — configured in [tasks.<lang>]. " +
		"It executes only the command the user saved for this workspace's language (no shell, no agent-supplied command line); the optional target fills a {target} placeholder with one shell-safe argument. " +
		"A project-supplied (.plumb/config.toml) command must be trusted first (run `plumb trust`); the shipped defaults and global-config commands always run. Output and runtime are bounded. " +
		"Pairs with topology_affected (which says WHICH tests to run; this runs them)."
}

type runTaskArgs struct {
	Slot   string `json:"slot"`
	Target string `json:"target"`
}

func (a runTaskArgs) validate() error {
	if !taskSlots[a.Slot] {
		return fmt.Errorf("run_task: slot must be one of build, lint, test, e2e, verify; got %q", a.Slot)
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
		return "", fmt.Errorf("run_task: task commands are not available for this session")
	}
	cmd, err := t.resolve(a.Slot, a.Target)
	if err != nil {
		return "", err
	}
	if len(cmd.Steps) == 0 {
		return "", fmt.Errorf("run_task: no %s command configured for this workspace", a.Slot)
	}
	return t.run(ctx, cmd)
}

func (t *Tasks) workspace() string {
	if t.deps.WorkspaceFn == nil {
		return ""
	}
	return t.deps.WorkspaceFn()
}

// run executes each step in sequence, stopping at the first non-zero exit, and
// renders a compact report.
func (t *Tasks) run(ctx context.Context, cmd TaskCommand) (string, error) {
	ws := t.workspace()
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
		b.WriteString(out + "\n")
	}
	if errOut := strings.TrimSpace(res.Stderr); errOut != "" {
		b.WriteString(errOut + "\n")
	}
	return b.String()
}
