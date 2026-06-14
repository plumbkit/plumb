package cli

// conn_config.go — per-project config apply/watch, client identity, and the
// shared write-budget binding.

import (
	"os"
	"path/filepath"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
)

// applyProjectConfig loads <workspace>/.plumb/config.toml and applies it to
// the live session (rate limit, strict mode, walk config).
func (s *connSession) applyProjectConfig(workspace string) {
	if workspace == "" {
		return
	}
	base := s.store.Current()
	projectCfg, err := config.LoadProject(base, workspace)
	if err != nil {
		s.log().Warn("daemon: project config invalid; using global", "workspace", workspace, "err", err)
		return
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
		v.edits = projectCfg.Edits
		v.walk = projectCfg.Walk
		v.git = projectCfg.Git
		v.ws = projectCfg.Workspace
		v.semantics = projectCfg.Semantics
		v.memory = projectCfg.Memory
		v.tasks = projectCfg.Tasks
		v.agentConfigWrites = projectCfg.AgentConfigWrites
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
		projectCfg.Git.AllowPush != base.Git.AllowPush {
		s.log().Info("daemon: project config applied",
			"workspace", workspace,
			"strict", projectCfg.Edits.Strict,
			"rate_limit_per_minute", projectCfg.Edits.RateLimitPerMinute,
			"refuse_home_roots", projectCfg.Walk.RefuseHomeRoots,
			"git.allow_writes", projectCfg.Git.AllowWrites,
			"git.allow_destructive", projectCfg.Git.AllowDestructive,
			"git.allow_push", projectCfg.Git.AllowPush)
	}
	// The workspace is now known (attach / re-pin / reload all funnel here), so
	// link the per-(client, workspace) shared write budget. Idempotent.
	s.bindWriteLimiterParent()
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
	session.SetClient(s.sessID, name, version)
	// Client identity may arrive before or after the workspace is pinned; bind
	// here too so the shared budget links as soon as both are known.
	s.bindWriteLimiterParent()
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
