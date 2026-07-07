package cli

// conn_commands.go wires run_command and execute_shell_command to the session:
// it resolves a [[command]] allow-list entry (or the shell policy) for the
// workspace and applies the per-workspace trust gate. Mirrors conn_tasks.go: a
// project-supplied command — and a project raising [commands] allow_shell — runs
// only after `plumb trust`; global-config commands and policy are user-authored
// and always honoured. The untrusted project's config is never forced back in
// LoadProject (that is reserved for fields with no per-call gate); the gate lives
// here, where the resolver can consult the trust store.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// commandResolver resolves a [[command]] name (+ optional target) to a runnable,
// sandboxed command for this session's workspace, applying the trust gate.
func (s *connSession) commandResolver(name, target string) (tools.ResolvedCommand, error) {
	ws := s.workspace()
	if ws == "" {
		return tools.ResolvedCommand{}, fmt.Errorf("run_command: no workspace is attached")
	}
	v := s.view()
	cmd, ok := config.FindCommand(v.commands, name)
	if !ok {
		avail := config.CommandNames(v.commands)
		if len(avail) == 0 {
			return tools.ResolvedCommand{}, fmt.Errorf("run_command: no commands are configured for this workspace; add a [[command]] entry to your global config or .plumb/config.toml")
		}
		return tools.ResolvedCommand{}, fmt.Errorf("run_command: unknown command %q; available: %s", name, strings.Join(avail, ", "))
	}
	fromProject := commandsFromProject(ws)
	if fromProject && !config.NewTrustStore().IsTrusted(ws) {
		return tools.ResolvedCommand{}, fmt.Errorf(
			"run_command: %q comes from this project's .plumb/config.toml and is not trusted. "+
				"review it, then run `plumb trust` in %s to allow this project's commands", name, ws)
	}
	argv, err := substituteTarget(cmd.Exec, target)
	if err != nil {
		return tools.ResolvedCommand{}, fmt.Errorf("run_command %q: %w", name, err)
	}
	provenance := "global"
	if fromProject {
		provenance = "project"
	}
	return tools.ResolvedCommand{
		Name:       name,
		Argv:       argv,
		WorkingDir: commandWorkdir(ws, cmd.WorkingDir),
		Timeout:    cmd.Timeout.Duration,
		Sandbox: tools.SandboxOpts{
			WorkspaceRoot: ws,
			AllowWrites:   cmd.AllowWrites,
			DenyNetwork:   cmd.DenyNetwork,
		},
		RequireSandbox: s.effectiveRequireSandbox(),
		Provenance:     provenance,
	}, nil
}

// shellResolver resolves the execute_shell_command policy for this workspace,
// applying the trust gate to a project that enables shell execution.
func (s *connSession) shellResolver() (tools.ResolvedShell, error) {
	ws := s.workspace()
	if ws == "" {
		return tools.ResolvedShell{}, fmt.Errorf("execute_shell_command: no workspace is attached")
	}
	base := s.store.Current()
	v := s.view()
	trusted := config.NewTrustStore().IsTrusted(ws)
	// An untrusted project's .plumb/config.toml cannot widen shell access: honour
	// the merged (project) value only when the workspace is trusted, else fall
	// back to the global base value.
	allowShell := base.CommandPolicy.AllowShell
	if trusted {
		allowShell = v.commandPolicy.AllowShell
	}
	if !allowShell {
		if !trusted && v.commandPolicy.AllowShell {
			return tools.ResolvedShell{}, fmt.Errorf(
				"execute_shell_command: this project's .plumb/config.toml enables shell execution, but the workspace is not trusted. "+
					"review it, then run `plumb trust` in %s", ws)
		}
		return tools.ResolvedShell{}, fmt.Errorf(
			"execute_shell_command is disabled. enable it with [commands] allow_shell = true in your global config, " +
				"or in this project's .plumb/config.toml plus `plumb trust`")
	}
	return tools.ResolvedShell{
		WorkingDir: ws,
		Sandbox: tools.SandboxOpts{
			WorkspaceRoot: ws,
			// The shell tier is trusted and opt-in, so workspace writes are expected
			// (formatters, code generators). The sandbox still confines writes away
			// from the rest of the filesystem.
			AllowWrites: true,
		},
		RequireSandbox: s.effectiveRequireSandbox(),
	}, nil
}

// effectiveRequireSandbox is the most-restrictive require_sandbox across the
// global base and the project value: an untrusted project can only ADD safety
// (raise require_sandbox), never lower it.
func (s *connSession) effectiveRequireSandbox() bool {
	return s.store.Current().CommandPolicy.RequireSandbox || s.view().commandPolicy.RequireSandbox
}

// commandsFromProject reports whether the workspace's .plumb/config.toml defines
// the [[command]] array, so its entries are project-sourced and need trust.
func commandsFromProject(ws string) bool {
	present, _ := config.ProjectValuePresent(ws, []string{"command"})
	return present
}

// commandWorkdir resolves a command's working_dir (validated relative and
// non-escaping at load) to an absolute path within the workspace.
func commandWorkdir(ws, dir string) string {
	if dir == "" || dir == "." {
		return ws
	}
	return filepath.Join(ws, dir)
}
