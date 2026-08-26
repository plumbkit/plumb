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
//
// A config that will not parse resolves to the GLOBAL config, and is applied.
// It used to return here instead, which read as "change nothing" but meant
// "keep whatever was applied last" — and on a re-pin what was applied last
// belongs to the PREVIOUS workspace. A session pinned to a trusted project and
// then re-pinned into another carried that project's allow_push and
// allow_destructive into it, so a repository could inherit a git tier it was
// never granted by shipping malformed TOML. That inverts the trust gate: the
// gate exists because a cloned repository ships a .plumb/config.toml, and this
// handed the destructive tier to one that ships a BROKEN one, with no
// `plumb trust` against it anywhere.
//
// LoadProjectWithPolicy already returns the base config on error, so falling
// through applies the global config in full — not just [git], but every block
// the previous project could have set.
func (s *connSession) applyProjectConfig(workspace string) {
	if workspace == "" {
		return
	}
	base := s.store.Current()
	projectCfg, policy, err := config.LoadProjectWithPolicy(base, workspace)
	unreadable := err != nil
	if unreadable {
		s.log().Warn("daemon: project config invalid; using global", "workspace", workspace, "err", err)
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
	// Same bytes again, and for the same reason: session_start's ignored-[git]
	// notice must describe the request that produced the policy below it, not
	// whatever a fresh read of the trust store says when the tool is later called.
	// The trust store is written by `plumb trust` and watched by nothing, so a
	// re-read would flip the notice to silent while the cached policy stayed
	// global — leaving `Push/fetch/pull: off.` with no explanation and the tool
	// still refusing, which is the very bug the notice exists to prevent, reached
	// by obeying it.
	projectGit := projectGitStatusOf(policy)
	if unreadable {
		// Record the skip. An unparseable project config is ignored WHOLE — its
		// [git] block as thoroughly as an untrusted one — and saying nothing here
		// leaves the agent with less to go on than the untrusted case, since there
		// is not even a `plumb trust` to reach for.
		projectGit = tools.ProjectGitStatus{Unreadable: true}
	}
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
		// Diff BEFORE the swap: a change in the collaboration capability switches
		// is surfaced to the agent on its next tool result / session_start, so a
		// newly granted (or revoked) mailbox / cross-project consent is never
		// silent (PLAN-414).
		if notice := collabChangeNotice(v.collab, projectCfg.Collab); notice != "" {
			v.collabNotice = notice
		}
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
		v.projectGit = projectGit
		if !cfgMtime.IsZero() {
			v.lastCfgMtime = cfgMtime
		}
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
	//
	// NOT on the unreadable path.
	//
	// The lasting damage is not the latch. s.xcodeStarted is per-CONNECTION and
	// dies with it, and the pool's running map is an in-flight singleflight guard
	// that pool_xcode deletes when the flow completes — a later session really does
	// re-run for the same root, which TestConnXcodeConfigAppliesOnNextSession
	// pins. The damage is on DISK: xcodebsp.Configure short-circuits on
	// BuildServerOK, so once a buildServer.json exists it is never regenerated by
	// any session on any daemon until someone deletes the file. Writing one from
	// the GLOBAL scheme, because THIS project's config failed to parse, is a
	// one-way action taken on a guess — and it carries a SourceKit-LSP restart.
	//
	// It needs a bare Xcode project, a trusted workspace and global
	// auto_build_server (default off) to reach the write at all. Narrow, but the
	// guard is not unreachable — do not remove it on the assumption that it is.
	//
	// Falling back to the global config is right for POLICY, which is a question
	// with an answer either way and is re-asked whenever the file's mtime changes.
	// It is wrong for an irreversible action, which is better not taken.
	//
	// A DELETED config still reaches this, and the asymmetry is deliberate:
	// "there is no project config" is a knowable state whose answer really is the
	// global one, while "the config says something we could not read" is a guess
	// about content that exists.
	if !unreadable {
		s.startXcodeForWorkspace(workspace, projectCfg.Xcode, execTrusted)
	}
	// Register this workspace with the daemon's per-workspace project-config
	// watcher (PLAN-414): attach, re-pin and every reload funnel through here,
	// so this is the one place the watcher set follows the pin. Idempotent on a
	// same-workspace reload; acquire-before-release on a re-pin.
	s.trackProjectWatch(workspace)

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

// projectGitStatusOf converts a resolved project-policy status into the shape
// session_start renders. Pure, so the capture at config apply is the only place
// the answer is decided.
func projectGitStatusOf(st config.ProjectPolicyStatus) tools.ProjectGitStatus {
	out := tools.ProjectGitStatus{Trusted: st.Trusted}
	for _, e := range st.Spec {
		out.Keys = append(out.Keys, tools.ProjectGitKey{Key: e.Key, Value: e.Value})
	}
	return out
}

// isStrict reports whether strict mode is in effect for this session.
func (s *connSession) isStrict() bool {
	return s.view().edits.Strict
}

// editsConfig returns the current resolved edits config.
func (s *connSession) editsConfig() config.EditsConfig {
	return s.view().edits
}

// memoryConfig returns the current resolved [memory] config off the lock-free
// snapshot (seeded at construction from global config, swapped per project on
// every attach / re-pin / reload). Lets the hot read_file hint path read the
// config without re-reading and re-parsing .plumb/config.toml per call.
func (s *connSession) memoryConfig() config.MemoryConfig {
	return s.view().memory
}

// collabConfig returns the connection's snapshotted, project-resolved [collab]
// config. Like memoryConfig it is captured on every attach / re-pin / reload, so
// the hot peer-hint path reads it without re-parsing .plumb/config.toml per call.
func (s *connSession) collabConfig() config.CollabConfig {
	return s.view().collab
}

// toolsConfig returns the current resolved [tools] config off the lock-free
// snapshot. Read on the tools/list filter path so the profile resolves without
// a per-call disk read; swapped per project like the blocks above.
func (s *connSession) toolsConfig() config.ToolsConfig {
	return s.view().tools
}

// refuseHomeRoots reports whether the session refuses home-directory roots.
func (s *connSession) refuseHomeRoots() bool {
	return s.view().walk.RefuseHomeRoots
}

// gitConfig returns the current resolved git tool config.
func (s *connSession) gitConfig() config.GitConfig {
	return s.view().git
}

// gitPolicy returns the connection's current resolved git policy. Reads the
// live git config off the lock-free snapshot (hot-reloaded via mutate) and is
// the single source of truth shared by the git tool's gate and session_start's
// policy report.
func (s *connSession) gitPolicy() tools.GitPolicy {
	return gitPolicyFrom(s.gitConfig())
}

// projectGitStatus is session_start's view of the state logProjectPolicy writes
// to the daemon log — the log reaches the user, this reaches the AGENT, which is
// the surface that was missing.
//
// It is served from the same snapshot as gitPolicy above, and deliberately does
// no lookup of its own: the two are captured together by applyProjectConfig, so
// what the notice claims about the policy and what the policy actually is cannot
// drift apart between them.
func (s *connSession) projectGitStatus() tools.ProjectGitStatus {
	return s.view().projectGit
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
		if !os.IsNotExist(err) {
			// A transient stat failure (a permissions blip, a busy filesystem) is not
			// evidence the file is gone, and revoking on it would flap the session's
			// policy for a reason that has nothing to do with the project.
			return
		}
		// The file was DELETED. Revoking matters as much here as on a parse
		// failure: without it, removing .plumb/config.toml leaves every override it
		// had granted — including [collab] cross_project consent and the git tier —
		// in force for the life of the session, so the way to keep an elevated
		// policy is to delete the file that justified it.
		//
		// Revoke once and then stay quiet: applyProjectConfig cannot stamp a
		// mtime for a file that is not there, so lastCfgMtime is cleared here and a
		// zero value marks "nothing applied", which every later poll reads as
		// already handled. A file that reappears has a non-zero mtime, which does
		// not equal the zero value, so it reloads normally.
		hadProjectConfig := false
		s.mutate(func(v *sessionView) {
			hadProjectConfig = !v.lastCfgMtime.IsZero()
			if hadProjectConfig {
				v.lastCfgMtime = time.Time{}
			}
		})
		if !hadProjectConfig {
			return
		}
		s.applyProjectConfig(workspace)
		s.log().Info("daemon: project config removed; reverted to global", "workspace", workspace)
		return
	}
	mtime := info.ModTime()
	alreadySeen := false
	s.mutate(func(v *sessionView) {
		alreadySeen = mtime.Equal(v.lastCfgMtime)
		if !alreadySeen {
			v.lastCfgMtime = mtime
		}
	})
	if alreadySeen {
		return
	}
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
	session.SetClient(s.sessionID(), name, version)
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
	session.SetProtocol(s.sessionID(), offered, answered, string(caps))
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
		WriteTimeout:      c.WriteTimeout.Duration,
	}
}
