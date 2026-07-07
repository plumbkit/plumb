package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// execute_shell_command.go is the execute_shell_command MCP tool: it runs an
// arbitrary agent-supplied command string through the shell (sh -c). This is the
// one place agent free-text reaches a command line, so it is DISABLED by default
// and gated hard: the cli seam refuses unless [commands] allow_shell is enabled
// (a project raising it needs `plumb trust`). Being the highest-risk tier, it
// leans on the OS sandbox — the resolver reports the confinement and whether an
// inactive sandbox must abort the run (require_sandbox).
//
// Concurrency: Execute is safe for concurrent use (no shared mutable state).

// ResolvedShell is the gate decision plus the run parameters for a shell command:
// the working directory, timeout, sandbox confinement, and whether an inactive
// sandbox must abort the run. The resolver returns an error instead when the
// shell tier is disabled or untrusted.
type ResolvedShell struct {
	WorkingDir     string
	Timeout        time.Duration
	Sandbox        SandboxOpts
	RequireSandbox bool
}

// ShellResolverFn resolves the shell-execution policy for the session's
// workspace. It errors — with actionable guidance — when execute_shell_command
// is disabled or when a project enabled it without trust. nil ⇒ the tool reports
// shell execution is unavailable.
type ShellResolverFn = func() (ResolvedShell, error)

// ExecuteShellCommand is the execute_shell_command MCP tool. It takes no
// filesystem path argument — the working directory comes from the resolver and
// confinement is the OS sandbox, not the path-boundary guard.
type ExecuteShellCommand struct {
	resolve ShellResolverFn
}

func NewExecuteShellCommand(resolve ShellResolverFn) *ExecuteShellCommand {
	return &ExecuteShellCommand{resolve: resolve}
}

var executeShellCommandSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "The shell command to run, executed via sh -c so pipes, redirects and globs work (e.g. \"go test ./... | tail -5\"). Runs in the workspace under an OS sandbox when available."
    }
  },
  "required": ["command"],
  "additionalProperties": false
}`)

func (t *ExecuteShellCommand) Name() string                 { return "execute_shell_command" }
func (t *ExecuteShellCommand) InputSchema() json.RawMessage { return executeShellCommandSchema }
func (t *ExecuteShellCommand) Description() string {
	return "Run an ad-hoc shell command in the workspace via sh -c (pipes/redirects/globs work), for verifying an edit compiles or tests pass without leaving plumb. " +
		"DISABLED by default: enable it with [commands] allow_shell = true in your global config, or in a project's .plumb/config.toml plus `plumb trust`. " +
		"Runs under an OS sandbox when available, but that sandbox is INTEGRITY-ONLY: it confines writes, not reads — the command runs with the user's credentials and the daemon's environment, so it can read any file and secret the user can (e.g. ~/.ssh, API keys) and reach the network unless [commands] deny_network is set. Enable it only for repositories you trust. Output and runtime are bounded. " +
		"Prefer run_command for anything you run repeatedly — a named allow-list entry is safer and needs no enabling."
}

type executeShellArgs struct {
	Command string `json:"command"`
}

func (t *ExecuteShellCommand) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a executeShellArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("execute_shell_command: invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", fmt.Errorf("execute_shell_command: command is required")
	}
	if t.resolve == nil {
		return "", fmt.Errorf("execute_shell_command: shell execution is not available for this session")
	}
	rs, err := t.resolve()
	if err != nil {
		return "", err
	}
	argv := []string{"sh", "-c", a.Command}
	wrapped, status := Sandbox(argv, rs.Sandbox)
	if rs.RequireSandbox && !status.Active {
		return "", fmt.Errorf("execute_shell_command: require_sandbox is set but no OS sandbox is active (%s); refusing to run unsandboxed", status.Reason)
	}
	start := time.Now()
	res, err := RunArgv(ctx, rs.WorkingDir, wrapped, rs.Timeout)
	if err != nil {
		return "", fmt.Errorf("execute_shell_command: %w", err)
	}
	elapsed := time.Since(start)

	var b strings.Builder
	fmt.Fprintf(&b, "execute_shell_command (sandbox=%s)\n", status)
	fmt.Fprintf(&b, "$ %s\n", a.Command)
	if out := strings.TrimSpace(res.Stdout); out != "" {
		b.WriteString(out + "\n")
	}
	if errOut := strings.TrimSpace(res.Stderr); errOut != "" {
		b.WriteString(errOut + "\n")
	}
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
