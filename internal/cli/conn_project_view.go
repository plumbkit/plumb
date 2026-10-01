package cli

// conn_project_view.go — the project-scoped config a CALL reads, for a call
// whose logical agent holds a root of its own (#522).
//
// applyProjectConfig swaps the connection's project blocks into the sessionView
// on attach, re-pin and reload — for the CONNECTION's root. An agent that pinned
// its own shard elsewhere got a working directory from its shard and everything
// else from the connection: its run_task ran project A's [tasks.<lang>]
// commands, working_dir and env against project B's root, and its run_command
// ran A's [[command]] allow-list under A's exec trust.

import (
	"context"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
)

// projectViewFor returns the session view whose project-scoped exec blocks —
// [tasks.<lang>], [[command]], [command_policy] and the exec trust resolved with
// them — describe the calling agent's effective workspace, and that workspace.
//
// A call on the root the connection's view was LOADED for gets that view
// untouched: a single-agent connection, an unattributed call, and an agent
// pinned to the connection's project behave exactly as before. Every other root
// has those blocks resolved for it. The test is configRoot, not acquiredRoot: a
// re-pin moves acquiredRoot before applyProjectConfig swaps the new root's
// config in, and keying on acquiredRoot handed a call in that window the new
// root paired with the old project's commands and trust. A view no project
// config was ever applied to holds only the global config, which no root can
// borrow anything from, so it is used as is.
//
// That resolution is done per call, from disk, rather than cached on the shard.
// The calls that read it (run_task, mutation_test, run_command, and the
// orientation that describes them) are rare next to the commands they start, a
// per-call load cannot go stale when the agent's config is edited, and it needs
// none of the watch/reload machinery the connection's view carries. It keeps the
// property conn_commands.go relies on: the commands and the exec trust that
// gates them come from ONE load, so the gate authorises the content it runs.
func (s *connSession) projectViewFor(ctx context.Context) (sessionView, string) {
	ws := s.workspaceFor(ctx)
	v := s.view()
	if ws == "" || ws == v.configRoot || (v.configRoot == "" && ws == v.acquiredRoot) {
		return v, ws
	}
	lang := v.acquiredLanguage
	if sh := s.shardFor(ctx); sh != nil {
		sh.mu.RLock()
		lang = sh.language
		sh.mu.RUnlock()
	}
	// As applyProjectConfig: an unreadable config resolves to the GLOBAL config,
	// never to whatever was applied last — here, the connection's project, whose
	// grants must not reach a repository that ships malformed TOML.
	cfg, policy, err := config.LoadProjectWithPolicy(s.store.Current(), ws)
	if err != nil {
		s.log().Warn("daemon: agent project config invalid; using global", "workspace", ws, "err", err)
	}
	v.acquiredRoot = ws
	v.acquiredLanguage = lang
	v.tasks = cfg.Tasks
	v.commands = cfg.Commands
	v.commandPolicy = cfg.CommandPolicy
	v.execTrusted = config.ExecTrustedFor(ws, policy)
	v.projectCommands = policy.Asked("command")
	// Detected for the connection's root, so not a statement about this one:
	// reporting them would list another project's languages as unreachable here.
	v.discoveredLangs = nil
	return v, ws
}

// taskWorkingDirSource names the setting a [tasks.<lang>] working_dir comes
// from — the project config when the project sets it, else the global config —
// so a working directory that turns out not to exist is refused naming the line
// to fix. "" when no working_dir is set.
func taskWorkingDirSource(ws, lang, dir string) string {
	if dir == "" || dir == "." {
		return ""
	}
	path := config.GlobalConfigPath()
	if cmds, err := config.ProjectTaskCommands(ws); err == nil {
		for _, c := range cmds {
			if strings.EqualFold(c.Lang, lang) && strings.EqualFold(c.Slot, taskWorkingDirKey) {
				path = config.ProjectConfigPath(ws)
				break
			}
		}
	}
	return fmt.Sprintf("[tasks.%s] working_dir = %q in %s", lang, dir, path)
}
