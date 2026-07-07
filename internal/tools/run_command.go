package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// run_command.go is the run_command MCP tool: it runs ONE entry from the
// [[command]] allow-list, resolved by name. The argv is fixed in the config —
// never built from agent free-text — so no injected string can reach the command
// line. The only agent-supplied value is an optional {target}, bounded to one
// shell-safe argument (targetPattern, shared with run_task) and substituted by
// the resolver. Resolution + the per-workspace trust gate live at the cli seam
// (the resolver closure); this file is the MCP surface, the sandbox wrap, and
// the bounded execution.
//
// Concurrency: Execute is safe for concurrent use (no shared mutable state).

// ResolvedCommand is a ready-to-run allow-list entry: the final argv (with
// {target} already substituted), the absolute working directory, the timeout,
// the sandbox confinement, and whether an inactive sandbox must abort the run.
type ResolvedCommand struct {
	Name           string
	Argv           []string
	WorkingDir     string
	Timeout        time.Duration
	Sandbox        SandboxOpts
	RequireSandbox bool
	Provenance     string // "global" | "project"
}

// CommandResolverFn resolves a command name (+ optional target) to a runnable
// command for the session's workspace, applying the per-workspace trust gate. It
// errors when the name is unknown, when a project-supplied command is not yet
// trusted, or when the {target} does not fit the command. nil ⇒ the tool reports
// command execution is unavailable.
type CommandResolverFn = func(name, target string) (ResolvedCommand, error)

// RunCommand is the run_command MCP tool. It takes no filesystem path argument —
// the workspace and working directory come from the resolver, and confinement is
// the OS sandbox, not the path-boundary guard.
type RunCommand struct {
	resolve CommandResolverFn
}

func NewRunCommand(resolve CommandResolverFn) *RunCommand {
	return &RunCommand{resolve: resolve}
}

var runCommandSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "The name of an entry in the [[command]] allow-list (in global or project .plumb/config.toml). You cannot pass an arbitrary command line — only a configured name."
    },
    "target": {
      "type": "string",
      "description": "Optional value substituted for the single {target} token in the command's fixed argv (e.g. a test name or package). Restricted to one shell-safe argument ([A-Za-z0-9._/:@-]); refused if the command has no {target}."
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`)

func (t *RunCommand) Name() string                 { return "run_command" }
func (t *RunCommand) InputSchema() json.RawMessage { return runCommandSchema }
func (t *RunCommand) Description() string {
	return "Run a named command from the workspace's [[command]] allow-list (build/test/lint/scripts) without leaving plumb. " +
		"It runs only the exact fixed argv the user configured (no shell, no agent-supplied command line); the optional target fills a single {target} placeholder with one shell-safe argument. " +
		"A command from a project's .plumb/config.toml must be trusted first (run `plumb trust`); a command from your global config always runs. " +
		"The command runs under an OS sandbox (a write jail) when one is available. Output and runtime are bounded. " +
		"Use execute_shell_command instead only for an ad-hoc command not worth adding to the allow-list (it must be enabled first)."
}

type runCommandArgs struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

func (a runCommandArgs) validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("run_command: name is required (the name of a [[command]] allow-list entry)")
	}
	if a.Target != "" && !targetPattern.MatchString(a.Target) {
		return fmt.Errorf("run_command: target %q is not a single shell-safe argument ([A-Za-z0-9._/:@-])", a.Target)
	}
	return nil
}

func (t *RunCommand) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a runCommandArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("run_command: invalid arguments: %w", err)
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	if t.resolve == nil {
		return "", fmt.Errorf("run_command: command execution is not available for this session")
	}
	rc, err := t.resolve(a.Name, a.Target)
	if err != nil {
		return "", err
	}
	return runResolved(ctx, rc)
}

// runResolved wraps the argv in the OS sandbox (honouring require_sandbox) and
// runs it, rendering a compact report.
func runResolved(ctx context.Context, rc ResolvedCommand) (string, error) {
	wrapped, status := Sandbox(rc.Argv, rc.Sandbox)
	if rc.RequireSandbox && !status.Active {
		return "", fmt.Errorf("run_command %q: require_sandbox is set but no OS sandbox is active (%s); refusing to run unsandboxed", rc.Name, status.Reason)
	}
	start := time.Now()
	res, err := RunArgv(ctx, rc.WorkingDir, wrapped, rc.Timeout)
	if err != nil {
		return "", fmt.Errorf("run_command %q: %w", rc.Name, err)
	}
	elapsed := time.Since(start)

	var b strings.Builder
	fmt.Fprintf(&b, "run_command %s (source=%s, sandbox=%s, network=%s)\n", rc.Name, rc.Provenance, status, networkLabel(rc.Sandbox.DenyNetwork))
	if rc.Sandbox.DenyNetwork {
		b.WriteString("network: OFF for this command (deny_network is set on its [[command]] entry).\n")
	}
	b.WriteString(formatStep(rc.Argv, res))
	switch {
	case res.TimedOut:
		fmt.Fprintf(&b, "→ timed out after %s\n", elapsed.Round(time.Millisecond))
	case res.ExitCode == 0:
		fmt.Fprintf(&b, "→ ok (%s)\n", elapsed.Round(time.Millisecond))
	default:
		fmt.Fprintf(&b, "→ exit %d (%s)\n", res.ExitCode, elapsed.Round(time.Millisecond))
	}
	return b.String(), nil
}
