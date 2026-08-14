package cli

// conn_config.go — per-project config apply/watch, client identity, and the
// shared write-budget binding.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tools"
)

// applyProjectConfig loads <workspace>/.plumb/config.toml and applies it to
// the live session (rate limit, strict mode, walk config).
func (s *connSession) applyProjectConfig(workspace string) {
	if workspace == "" {
		return
	}
	base := s.store.Current()
	projectCfg, policy, err := config.LoadProjectWithPolicy(base, workspace)
	if err != nil {
		s.log().Warn("daemon: project config invalid; using global", "workspace", workspace, "err", err)
		projectCfg = base
		policy = config.ProjectPolicyStatus{}
	}
	s.logProjectPolicy(workspace, policy)
	// Resolved here, from the same load as projectCfg, and stored in the view: the
	// command/shell/xcode gates must authorise the exact content they are about to
	// run, not whatever the file says when the tool is later called.
	execTrusted := config.ExecTrustedFor(workspace, policy)
	// Provenance from the same spec, so `[[COMMAND]]` is recognised as
	// project-supplied. Asked() compares case-insensitively and the spec is built
	// with rawValues, so no spelling escapes it. Deriving it here rather than
	// re-reading the file is what closed the case-fold bypass — see conn_commands.go.
	projectCommands := policy.Asked("command")
	configPath := filepath.Join(workspace, ".plumb", "config.toml")
	var cfgMtime time.Time
	if info, statErr := os.Stat(configPath); statErr == nil {
		cfgMtime = info.ModTime()
	}
	// One mutation: swap the four config blocks, seed the config mtime, and rebuild
	// the boundary policy eagerly (configured roots may have changed). muMutate
	// subsumes the former applyMu — the lane already serialises config apply across
	// attach / the 30s poll / the global-config subscription.
	s.mutate(func(v *sessionView) {
		v.edits = projectCfg.Edits
		v.walk = projectCfg.Walk
		v.git = projectCfg.Git
		v.ws = projectCfg.Workspace
		v.semantics = projectCfg.Semantics
		v.memory = projectCfg.Memory
		v.collab = projectCfg.Collab
		v.tools = projectCfg.Tools
		v.session = projectCfg.Session
		v.tasks = projectCfg.Tasks
		v.agentConfigWrites = projectCfg.AgentConfigWrites
		v.commands = projectCfg.Commands
		v.commandPolicy = projectCfg.CommandPolicy
		v.execTrusted = execTrusted
		v.projectCommands = projectCommands
		v.lastCfgMtime = cfgMtime
		v.policy = s.buildPathPolicy(v)
	})
	s.writeLimiter.SetLimit(projectCfg.Edits.RateLimitPerMinute)
	if projectCfg.Edits.Strict != base.Edits.Strict ||
		projectCfg.Edits.RateLimitPerMinute != base.Edits.RateLimitPerMinute ||
		projectCfg.Walk.RefuseHomeRoots != base.Walk.RefuseHomeRoots ||
		projectCfg.Git.AllowWrites != base.Git.AllowWrites ||
		projectCfg.Git.AllowDestructive != base.Git.AllowDestructive ||
		projectCfg.Git.AllowPush != base.Git.AllowPush ||
		projectCfg.Git.CommitTrailer != base.Git.CommitTrailer {
		s.log().Info("daemon: project config applied",
			"workspace", workspace,
			"strict", projectCfg.Edits.Strict,
			"rate_limit_per_minute", projectCfg.Edits.RateLimitPerMinute,
			"refuse_home_roots", projectCfg.Walk.RefuseHomeRoots,
			"git.allow_writes", projectCfg.Git.AllowWrites,
			"git.allow_destructive", projectCfg.Git.AllowDestructive,
			"git.allow_push", projectCfg.Git.AllowPush,
			"git.commit_trailer", projectCfg.Git.CommitTrailer)
	}
	// The workspace is now known (attach / re-pin / reload all funnel here), so
	// link the per-(client, workspace) shared write budget. Idempotent.
	s.bindWriteLimiterParent()
	// Xcode fields are next-session settings. The first config apply for a root on
	// this connection starts the pool-owned background flow; hot reloads do not
	// retroactively execute newly-enabled project tooling in an existing session.
	s.startXcodeForWorkspace(workspace, projectCfg.Xcode, execTrusted)
	// A project config may set [tools] profile = "full" (or per-client) — tell the
	// client to re-list when the resolved profile changed. Runs after the mutate
	// above has returned, so it holds no lock when it calls view()/mutate().
	s.maybeNotifyToolProfileChange()
}

// logProjectPolicy leaves the attach-time breadcrumb for a workspace whose
// project config asks for capability-granting settings ([git], the exec-deciding
// [lsp.<lang>] fields). Untrusted, those are silently forced back to the global
// config — correct, but a user debugging "my project config does nothing" needs
// something in the daemon log to find. Silent on the common case, where the
// project asks for none.
func (s *connSession) logProjectPolicy(workspace string, st config.ProjectPolicyStatus) {
	if st.Spec.IsEmpty() {
		return
	}
	if st.Trusted {
		s.log().Info("daemon: project capability config trusted and applied",
			"workspace", workspace, "keys", st.Spec.Keys())
		return
	}
	s.log().Warn("daemon: project capability config IGNORED (untrusted) — global values in force; run `plumb trust` to honour them",
		"workspace", workspace, "keys", st.Spec.Keys())
}

func (s *connSession) startXcodeForWorkspace(workspace string, cfg config.XcodeConfig, trusted bool) {
	if s.pool == nil || workspace == "" {
		return
	}
	root := canonicalXcodeRoot(workspace)
	s.xcodeStartedMu.Lock()
	if s.xcodeStarted == nil {
		s.xcodeStarted = make(map[string]bool)
	}
	if s.xcodeStarted[root] {
		s.xcodeStartedMu.Unlock()
		return
	}
	s.xcodeStarted[root] = true
	s.xcodeStartedMu.Unlock()
	s.pool.ensureXcodeBuildServer(root, cfg, trusted)
}

// maybeNotifyToolProfileChange emits notifications/tools/list_changed when the
// resolved tool profile differs from what was last advertised, so a client that
// honours the notification re-lists and picks up a mid-session [tools] profile
// change (e.g. a project config setting profile = "full"). A client that lists
// only once simply ignores it. No-op when the profile is unchanged or no
// notifier is wired (tests, pre-init).
func (s *connSession) maybeNotifyToolProfileChange() {
	v := s.view()
	if v.notify == nil {
		return
	}
	cur, _ := s.resolveToolProfile()
	if cur == v.lastToolProfile {
		return
	}
	s.mutate(func(v *sessionView) { v.lastToolProfile = cur })
	if err := v.notify("notifications/tools/list_changed", nil); err != nil {
		s.log().Debug("daemon: tools/list_changed notify failed", "err", err)
	}
}

// startConfigWatcher launches a background goroutine that polls for config file
// changes every 30 seconds and reapplies the config when the file is modified.
// The goroutine runs until s.ctx is cancelled (on session disconnect or daemon shutdown).
// Invoked exactly once per session via sync.Once.
func (s *connSession) startConfigWatcher() {
	s.watcherOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
					s.checkAndReloadConfig()
				}
			}
		}()
	})
}

// checkAndReloadConfig reapplies the workspace config when its file mtime
// differs from the last-applied version (lastCfgMtime, seeded at attach by
// applyProjectConfig). Any changed mtime triggers a reload — there is no
// staleness window, so edits made with a backdated mtime (git checkout,
// restore-from-backup) are still picked up. Called on each watcher poll.
func (s *connSession) checkAndReloadConfig() {
	workspace := s.workspace()
	if workspace == "" {
		return
	}
	configPath := filepath.Join(workspace, ".plumb", "config.toml")
	info, err := os.Stat(configPath)
	if err != nil {
		// A removed project config must revoke its overrides immediately. Other stat
		// failures also reapply the global base so an unreadable file cannot preserve
		// a previously granted capability in the live session view.
		if os.IsNotExist(err) && s.view().lastCfgMtime.IsZero() {
			return
		}
		s.applyProjectConfig(workspace)
		s.log().Info("daemon: project config removed or unreadable; global config reapplied", "workspace", workspace)
		return
	}
	mtime := info.ModTime()
	if mtime.Equal(s.view().lastCfgMtime) {
		return
	}
	// applyProjectConfig records the mtime only after it has either applied the
	// parsed project config or replaced an invalid one with the global base.
	s.applyProjectConfig(workspace)
	s.log().Info("daemon: project config hot-reloaded", "workspace", workspace)
}

// onClientInfo handles the MCP clientInfo notification: stores client identity,
// updates the session record, and links the shared client rate-limiter budget.
func (s *connSession) onClientInfo(name, version string) {
	s.mutate(func(v *sessionView) {
		v.clientName = name
		v.clientVersion = version
	})
	s.log().Info("daemon: client identified", "client", name, "version", version)
	session.SetClient(s.sessID, name, version)
	// Client identity may arrive before or after the workspace is pinned; bind
	// here too so the shared budget links as soon as both are known.
	s.bindWriteLimiterParent()
}

// onProtocolNegotiated records the initialize-time MCP protocol negotiation:
// it stores the offered/answered revisions and the client-advertised
// capabilities on the session view and persists them to the session record, so
// daemon_info and the TUI can show them. It also logs once when the offered
// revision differs from the answered one — the fleet-visibility signal for
// when moving the supported set forward is safe. Fires exactly once per
// connection, enforced by the once-guard in dispatchMessage.
func (s *connSession) onProtocolNegotiated(offered, answered string, caps json.RawMessage) {
	s.mutate(func(v *sessionView) {
		v.protocolOffered = offered
		v.protocolAnswered = answered
		v.clientCaps = append(json.RawMessage(nil), caps...)
	})
	if offered != "" && offered != answered {
		s.log().Info("daemon: client offered an MCP protocol revision plumb does not implement; answered with the newest supported",
			"offered", offered, "answered", answered, "client", s.clientNameStr())
	}
	session.SetProtocol(s.sessID, offered, answered, string(caps))
}

// protocolStatus returns the initialize-time protocol negotiation snapshot for
// daemon_info: the offered/answered revisions plus the flattened client
// capability keys. Zero value before the initialize exchange completes.
func (s *connSession) protocolStatus() tools.ProtocolStatus {
	v := s.view()
	return tools.ProtocolStatus{
		Offered:      v.protocolOffered,
		Answered:     v.protocolAnswered,
		Capabilities: flattenCapabilityKeys(v.clientCaps),
	}
}

// flattenCapabilityKeys renders client-advertised capabilities JSON as a sorted
// list of dotted keys ("roots.listChanged"), one level deep — enough to see
// what a client can do at a glance. Nil or malformed input yields nil.
func flattenCapabilityKeys(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	keys := make([]string, 0, len(top))
	for k, v := range top {
		var sub map[string]json.RawMessage
		if err := json.Unmarshal(v, &sub); err == nil && len(sub) > 0 {
			for sk := range sub {
				keys = append(keys, k+"."+sk)
			}
			continue
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// onAllowDirs records the extra read-write roots the client granted at
// connection time (serve --allow-dir / PLUMB_ALLOWED_DIRS, transported in the
// initialize params' _meta). It runs synchronously during the initialize
// exchange — before OnInit attaches the workspace — so the roots are present
// when buildPathPolicy first runs at attach. The rebuild here is a no-op while
// the session is still unattached (nil policy) and a belt-and-braces refresh
// otherwise. The grant is per-connection: it lives only in this session's view
// and is preserved across re-pins.
func (s *connSession) onAllowDirs(dirs []string) {
	if len(dirs) == 0 {
		return
	}
	s.mutate(func(v *sessionView) {
		v.allowDirs = dirs
		v.policy = s.buildPathPolicy(v)
	})
	s.log().Info("daemon: client granted extra read-write roots", "count", len(dirs))
}

// bindWriteLimiterParent links the session's write limiter to the budget shared
// by all connections from the same client identity working the SAME workspace.
//
// Keying on (client, workspace) — rather than client identity alone — preserves
// the anti-bypass guarantee within a project (a client cannot multiply its
// write budget by opening several connections to one workspace) while keeping
// different workspaces fully independent: a write burst in one project never
// throttles a sibling session in another. This is the cross-workspace isolation
// contract — two sessions on two different roots behave as isolated processes.
//
// No-op until both the client identity and the workspace are known. Writes
// cannot occur before a workspace is pinned (the boundary guard refuses them),
// so no shared budget is needed pre-attach. Safe to repeat, so it is called both
// on client-info and from applyProjectConfig (every attach / re-pin /
// config-reload path): a repeat call on the same key only refreshes the cap
// (tracking a config reload), while a re-pin acquires the new root's budget
// before releasing the old one, so the old entry is reclaimed once its last
// session leaves (see sharedBudgets) yet a re-pin back to a recently-left root
// never races teardown.
func (s *connSession) bindWriteLimiterParent() {
	if s.budgets == nil {
		return
	}
	v := s.view()
	name, version, root := v.clientName, v.clientVersion, v.acquiredRoot
	if name == "" || root == "" {
		return
	}
	// Track the same cap the per-session child currently enforces (applyProjectConfig
	// has already SetLimit'd it to the resolved project value), so a config reload
	// propagates to the shared budget instead of leaving it at its creation value.
	_, limit, _ := s.writeLimiter.Snapshot()
	key := name + "/" + version + "\x00" + root

	var prevKey string
	s.mutate(func(v *sessionView) {
		prevKey = v.boundBudgetKey
		v.boundBudgetKey = key
	})

	// Same key (a reload or a repeat bind on the same workspace): refresh the cap
	// without touching the refcount or re-parenting.
	if prevKey == key {
		s.budgets.setLimit(key, limit)
		return
	}
	// Acquire-before-release: pin the new budget before dropping the old so a
	// re-pin back to a recently-left key never reclaims it mid-flight.
	parent := s.budgets.acquire(key, limit)
	if prevKey != "" {
		s.budgets.release(prevKey)
	}
	s.writeLimiter.SetParent(parent)
}

// gitPolicyFrom adapts the resolved [git] config into the tools package's
// GitPolicy. It is the ONLY crossing between the two, and every field of [git]
// must make it across: a field dropped here is silently inert, with the config,
// `plumb config show` and the trust disclosure all still reporting it as set.
// TestGitPolicyFrom_CarriesEveryGitField guards exactly that.
//
// Env in particular must not be reconstructed from anywhere else — it is a
// capability (a git child's environment names commands git runs), and routing
// it through [git] is what puts it behind the project-config trust gate.
func gitPolicyFrom(c config.GitConfig) tools.GitPolicy {
	return tools.GitPolicy{
		AllowWrites:       c.AllowWrites,
		AllowDestructive:  c.AllowDestructive,
		AllowPush:         c.AllowPush,
		ProtectedBranches: c.ProtectedBranches,
		CommitTrailer:     c.CommitTrailer,
		Env:               c.Env,
	}
}
