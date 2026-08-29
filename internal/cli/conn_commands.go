package cli

// conn_commands.go wires run_command to the session: it resolves a [[command]]
// allow-list entry for the workspace and applies the per-workspace trust gate.
// Mirrors conn_tasks.go: a project-supplied command runs only after
// `plumb trust`; global-config commands are user-authored and always honoured.
// The untrusted project's config is never forced back in LoadProject (that is
// reserved for fields with no per-call gate); the gate lives here, where the
// resolver can distinguish a project entry from a global one.
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
