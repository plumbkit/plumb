package cli

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// This file holds the steps startOrReuse (pool.go) takes once it has resolved a
// workspace's language and config without the pool mutex: reusing or waking an
// existing entry, starting a new one, and attaching its file watcher after the
// unlock (PLAN-536).

// reuseUnlock hands back an existing entry — waking it when hibernated — and
// RELEASES p.mu, which the caller holds on entry. A woken entry's file watcher
// is created after the unlock.
func (p *workspacePool) reuseUnlock(e *poolEntry, pin, call bool) (*poolEntry, <-chan error, error) {
	// Pin only AFTER any fallible step (wakeLocked): pinning before a wake that
	// errors would leak a reference (refs incremented, no matching release) and
	// cancel the grace timer on an entry that stays hibernated.
	switch e.state {
	case poolHibernated:
		readyCh, err := p.wakeLocked(e)
		if err != nil {
			p.mu.Unlock()
			return nil, nil, err
		}
		p.leaseLocked(e, pin, call)
		slog.Info("pool: waking hibernated LS", "root", e.root, "language", e.language, "refs", e.refs)
		p.mu.Unlock()
		p.attachWatcher(e)
		return e, readyCh, nil
	case poolHibernating:
		// Teardown in flight; the server is not restartable until it settles.
		// Return the not-yet-ready entry so the caller retries (route surfaces
		// "LSP server not yet ready"); the next acquire finds it hibernated and
		// wakes it.
		p.leaseLocked(e, pin, call)
		p.mu.Unlock()
		return e, nil, nil
	default:
		p.leaseLocked(e, pin, call)
		slog.Info("pool: reusing LS", "root", e.root, "language", e.language, "refs", e.refs)
		p.mu.Unlock()
		return e, nil, nil
	}
}

// startLocked builds a new entry for key, starts its language server warming,
// and publishes it in the map. Caller holds p.mu. What stays under the lock is
// short: argsFor's small filesystem prep (the server's data directory — jdtls
// -data, kotlin-lsp --system-path — and its mtime stamp) and
// StartAsync, which spawns the process and its goroutine — keeping the spawn
// under the lock is what keeps one key from ever getting two servers. The file
// watcher's walk does not — the caller attaches it after the unlock
// (attachWatcher).
func (p *workspacePool) startLocked(key poolKey, lspCfg config.LSPConfig, env []string, goWorkOff string) (*poolEntry, <-chan error, error) {
	root, language := key.root, key.language
	// LRU eviction: before starting a new server, if this language is at its
	// max_workspaces budget, hibernate its least-recently-used running entry.
	// hibernateEntry re-takes p.mu and does blocking teardown, so it runs in a
	// goroutine that parks until the caller releases the lock; the new server
	// starts immediately and the victim's JVM is reclaimed concurrently.
	if victim := p.overBudgetVictimLocked(language, lspCfg.MaxWorkspaces); victim != nil {
		slog.Info("pool: max_workspaces reached — evicting LRU", "language", language, "victim_root", victim.root, "max", lspCfg.MaxWorkspaces)
		go p.hibernateEntry(victim)
	}

	rootURI := protocol.FileURI(root)
	c := cache.New(p.cacheTTL, p.cacheMaxSize)
	inv := cache.NewInvalidator(c)
	proxy := &clientProxy{}
	e := &poolEntry{root: root, language: language, lspCfg: lspCfg, proxy: proxy, inv: inv, cache: c, state: poolStarting, startedAt: time.Now(), goWorkOff: goWorkOff}
	proxy.onIdle = func() { go p.scheduleIdleReap(e) }
	proxy.touch()

	sup := lsp.NewSupervisor(lspCfg.Command, argsFor(language, root, lspCfg), env, lsp.SupervisorOptions{
		OnStart: p.poolOnStart(e, rootURI, lspCfg),
		// Run the server from the workspace it serves, not from the daemon's cwd
		// (which is "/"). Skipped when root is not an existing directory, so a root
		// deleted under a live session degrades to the daemon's cwd rather than
		// failing the spawn outright with ENOENT from chdir.
		Dir: existingDir(root),
	})
	// Warm up on the pool's base (daemon-lifetime) context, not a request ctx.
	readyCh, err := sup.StartAsync(p.baseCtx)
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("starting %s for %s: %w", language, root, err)
	}
	e.sup = sup
	p.entries[key] = e
	return e, readyCh, nil
}

// newWatcher builds a file watcher for root through the pool's factory.
func (p *workspacePool) newWatcher(root string, proxy *clientProxy) (*lspFSWatcher, error) {
	if p.watcherFactory != nil {
		return p.watcherFactory(root, proxy)
	}
	return newLSPFSWatcher(root, proxy)
}

// attachWatcher creates e's file watcher WITHOUT p.mu — its initial walk is the
// slowest step of a start or a wake — and publishes it under the lock only if
// the entry is still the live, running entry that has none and the pool is not
// closed. An entry torn down or hibernated meanwhile, or one close() has already
// swept (it does not empty p.entries), gets nothing, and the watcher is stopped.
func (p *workspacePool) attachWatcher(e *poolEntry) {
	watcher, err := p.newWatcher(e.root, e.proxy)
	if err != nil {
		slog.Warn("lsp: file watcher unavailable", "root", e.root, "language", e.language, "err", err)
		return
	}
	p.mu.Lock()
	live := !p.closed && p.entries[poolKey{e.root, e.language}] == e && e.state.running() && e.watcher == nil
	if live {
		e.watcher = watcher
		watcher.Start()
	}
	p.mu.Unlock()
	if !live {
		watcher.Stop()
	}
}
