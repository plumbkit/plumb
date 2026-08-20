package cli

// conn_commands.go wires run_command and execute_shell_command to the session:
// it resolves a [[command]] allow-list entry (or the shell policy) for the
// workspace and applies the per-workspace trust gate. Mirrors conn_tasks.go: a
// project-supplied command — and a project raising [commands] allow_shell — runs
// only after `plumb trust`; global-config commands and policy are user-authored
// and always honoured. The untrusted project's config is never forced back in
// LoadProject (that is reserved for fields with no per-call gate); the gate lives
// here, where the resolver can distinguish a project entry from a global one.
//
// The gate reads v.execTrusted, which config apply resolved from the same bytes
// that produced v.commands / v.commandPolicy. It is NOT a trust-store lookup at
// call time, and the difference matters twice over. It binds to CONTENT, so a
// [[command]] added or rewritten after `plumb trust` is refused until re-granted
// (before, the coarse per-root boolean blessed anything these sections later
// came to contain). And it binds to the content actually LOADED, so a repository
// cannot have hostile commands in the live config while the file on disk reads
// as something the user once approved.

import (
	"errors"
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
		return tools.ResolvedCommand{}, errors.New("run_command: no workspace is attached")
	}
	v := s.view()
	cmd, ok := config.FindCommand(v.commands, name)
	if !ok {
		avail := config.CommandNames(v.commands)
		if len(avail) == 0 {
			return tools.ResolvedCommand{}, errors.New("run_command: no commands are configured for this workspace; add a [[command]] entry to your global config " +
				"or .plumb/config.toml (a project-supplied entry also needs `plumb trust`). " +
				"For an ordinary build/lint/test, prefer run_task and its [tasks.<lang>] slots — those ship with defaults " +
				"and agent_config can set them when the user has enabled [agent_config_writes]")
		}
		return tools.ResolvedCommand{}, fmt.Errorf("run_command: unknown command %q; available: %s", name, strings.Join(avail, ", "))
	}
	fromProject := v.projectCommands
	if fromProject && !v.execTrusted {
		return tools.ResolvedCommand{}, fmt.Errorf(
			"run_command: %q comes from this project's .plumb/config.toml and is not trusted for its current content. "+
				"review it, then run `plumb trust` in %s to allow this project's commands "+
				"(a grant is bound to the exact content approved, so editing them requires re-running it)", name, ws)
	}
	argv, err := substituteTarget(cmd.Exec, target)
	if err != nil {
		return tools.ResolvedCommand{}, fmt.Errorf("run_command %q: %w", name, err)
	}
	provenance := "global"
	if fromProject {
		provenance = "project"
	}
	workdir, err := commandWorkdir(ws, cmd.WorkingDir)
	if err != nil {
		return tools.ResolvedCommand{}, fmt.Errorf("run_command %q: %w", name, err)
	}
	return tools.ResolvedCommand{
		Name:       name,
		Argv:       argv,
		WorkingDir: workdir,
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
		return tools.ResolvedShell{}, errors.New("execute_shell_command: no workspace is attached")
	}
	base := s.store.Current()
	v := s.view()
	trusted := v.execTrusted
	allowShell := gatedAllowShell(base.CommandPolicy, v.commandPolicy, trusted)
	if !allowShell {
		if !trusted && v.commandPolicy.AllowShell {
			return tools.ResolvedShell{}, fmt.Errorf(
				"execute_shell_command: this project's .plumb/config.toml enables shell execution, but its current content is not trusted. "+
					"review it, then run `plumb trust` in %s", ws)
		}
		return tools.ResolvedShell{}, errors.New("execute_shell_command is disabled. enable it with [commands] allow_shell = true in your global config, " +
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
			// The jail is integrity-only (reads stay permissive), so [commands]
			// deny_network is the egress control against a shell command that reads a
			// secret and exfiltrates it. On by default; trusted config may re-open it.
			DenyNetwork: gatedDenyNetwork(base.CommandPolicy, v.commandPolicy, trusted),
		},
		RequireSandbox: s.effectiveRequireSandbox(),
	}, nil
}

// gatedAllowShell applies the trust rule to execute_shell_command: an untrusted
// project's .plumb/config.toml cannot widen shell access, so the merged (project)
// value is honoured only when the workspace is trusted; otherwise the global base
// value wins. A project narrowing shell to false while untrusted is likewise not
// honoured (base wins) — an untrusted project can neither raise nor lower it.
func gatedAllowShell(base, merged config.CommandsConfig, trusted bool) bool {
	if trusted {
		return merged.AllowShell
	}
	return base.AllowShell
}

// gatedDenyNetwork applies the trust rule to the shell tier's deny_network
// (default true): the merged (project) value is honoured only when the workspace
// is trusted, otherwise the global base value wins. So an untrusted project can
// neither re-open the network (deny_network=false ignored) nor is its extra
// caution meaningful — a trusted user/project is the only way to allow egress.
func gatedDenyNetwork(base, merged config.CommandsConfig, trusted bool) bool {
	if trusted {
		return merged.DenyNetwork
	}
	return base.DenyNetwork
}

// effectiveRequireSandbox is the most-restrictive require_sandbox across the
// global base and the project value: an untrusted project can only ADD safety
// (raise require_sandbox), never lower it.
func (s *connSession) effectiveRequireSandbox() bool {
	return s.store.Current().CommandPolicy.RequireSandbox || s.view().commandPolicy.RequireSandbox
}

// (commandsFromProject removed.) Provenance now comes from the session view,
// resolved at config apply from the same bytes as v.commands and v.execTrusted.
//
// It used to call config.ProjectValuePresent(ws, []string{"command"}), which
// bottoms out in an EXACT map lookup of raw["command"]. That was a gate bypass,
// not a cosmetic issue: go-toml/v2 binds a table name to a struct field
// case-insensitively, so a cloned repository shipping `[[COMMAND]]` had its
// entry decoded into Config.Commands and returned by FindCommand, while the
// exact lookup missed the raw key — fromProject came back false, the trust gate
// was skipped entirely, and the argv ran as the user with provenance mislabelled
// "global". Arbitrary code execution from an untrusted clone, and the same
// defect class #243 closed for [lsp.<lang>].
//
// Deriving it from the policy spec fixes both halves: the spec is built with
// rawValues (case-insensitive, any value shape), and it is the spec computed
// during THIS session's config apply, so there is no second read of the file to
// disagree with the config actually loaded.

// commandWorkdir resolves a working_dir (validated relative and non-escaping at
// load) to an absolute path, REFUSING one that leaves the workspace.
//
// The load-time validator is lexical: it rejects an absolute path and a ".."
// segment, which is every escape you can spell. It is not every escape there is.
// `working_dir = "build"` passes it and still runs the command in /etc when
// build is a symlink, because the kernel resolves the link and filepath.Clean
// never sees it. That is the lexical-Clean-before-symlink-resolution class of
// bug, and the rule it taught is the one applied here: RE-CHECK after
// resolution, and REFUSE — never "clean" a path into something that looks
// contained, because the cleaned string and the directory the kernel opens are
// then two different places.
//
// PathWithinWorkspace does the resolution (it follows symlinks for an existing
// path and for the nearest existing ancestor), so this is the check the argv
// actually runs under, not a second opinion about the string.
func commandWorkdir(ws, dir string) (string, error) {
	if dir == "" || dir == "." {
		return ws, nil
	}
	abs := filepath.Join(ws, dir)
	if !tools.PathWithinWorkspace(ws, abs) {
		return "", fmt.Errorf("working_dir %q resolves to %s, which is outside the workspace %s "+
			"(a symlink out of the tree passes the relative-path check but not this one); refusing to run there", dir, abs, ws)
	}
	return abs, nil
}
