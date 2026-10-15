package cli

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// This file applies a changed [lsp.<lang>] config to the servers already
// running (PLAN-539). Before, any [lsp.*] edit only logged "config change
// requires a restart", and a whole-daemon restart re-warmed every project. Now a
// change restarts exactly the entries whose effective config it changes — that
// language's, under the roots it governs — and leaves every other server, and
// every project's warm state, alone.
//
// An entry is restarted IN PLACE through the hibernation lifecycle: it is
// hibernated once no call holds it (a call in progress is never cut short),
// given a new supervisor built from the new config, and woken again — at once
// when a session holds it, otherwise on its next acquire. Its leases, borrows,
// cache and map slot survive, so no session has to re-pin.

// reconfigurePoll is how often a restart retries the hibernation while calls
// hold the entry; reconfigureWarnEvery is how often it says it is still waiting.
const (
	reconfigurePoll      = 20 * time.Millisecond
	reconfigureWarnEvery = 30 * time.Second
)

// applyGlobalLSP adopts a new global config's [lsp.*] blocks and restarts the
// entries whose effective config they change. A language turned on with
// `plumb enable-lsp` stays on, unless this edit itself changed that language's
// `enabled` in the file — the later deliberate act wins. Returns how many
// entries it restarts.
//
// p.mu is held across the merge and the publish (p.mu → langsMu, the order
// enableLanguage uses), so an enable cannot land between them and be lost.
func (p *workspacePool) applyGlobalLSP(cfg config.Config) int {
	next := make(map[string]config.LSPConfig, len(cfg.LSP))
	maps.Copy(next, cfg.LSP)
	p.mu.Lock()
	for name := range p.liveEnabled {
		l, ok := next[name]
		if !ok {
			continue
		}
		if prev, had := p.fileLSP[name]; had && prev.Enabled != l.Enabled {
			delete(p.liveEnabled, name) // the file now decides; this edit is newer
			continue
		}
		l.Enabled = true
		next[name] = l
	}
	p.fileLSP = maps.Clone(cfg.LSP)
	// baseConfig.LSP is written only under p.mu and langsMu together, so p.mu
	// alone reads it: an unrelated reload stops here, before the LookPath
	// calls activeLanguages makes.
	if reflect.DeepEqual(p.baseConfig.LSP, next) {
		p.mu.Unlock()
		return 0
	}
	scoped := cfg
	scoped.LSP = next
	langs := activeLanguages(scoped)
	p.langsMu.Lock()
	p.baseConfig.LSP = next
	p.langs = langs
	p.langsMu.Unlock()
	p.mu.Unlock()
	p.invalidateAllLanguageConfigs()
	p.langsGen.Add(1)
	slog.Info("pool: global [lsp] config changed — restarting the affected language servers only")
	return p.reconfigureEntries(func(*poolEntry) bool { return true })
}

// reconfigureRoot restarts the entries under root whose effective config a
// project-config reload changed.
func (p *workspacePool) reconfigureRoot(root string) int {
	return p.reconfigureEntries(func(e *poolEntry) bool {
		return e.root == root || strings.HasPrefix(e.root, root+"/")
	})
}

// reconfigureEntries reconfigures every selected entry the pool started (one
// with a supervisor: startLocked sets it before publishing). Returns how many
// restarts it started.
func (p *workspacePool) reconfigureEntries(selected func(*poolEntry) bool) int {
	p.mu.Lock()
	var cands []*poolEntry
	for _, e := range p.entries {
		if e.sup != nil && selected(e) {
			cands = append(cands, e)
		}
	}
	p.mu.Unlock()
	n := 0
	for _, e := range cands {
		if p.reconfigureEntry(e) {
			n++
		}
	}
	return n
}

// reconfigureEntry brings one entry to its current effective config: adopt a
// knobs-only change in place, or start an in-place restart. The decision is
// made in one p.mu section. A request that arrives while a restart is already
// running marks it to run again, so the restart converges on the latest config
// instead of finishing on the one it started with. An entry governed by a
// project config that does not parse is left as it is until the file is fixed
// — the pool's fallback to the global config is a degradation, not an edit.
func (p *workspacePool) reconfigureEntry(e *poolEntry) bool {
	if p.projectConfigInvalid(e.root) {
		return false
	}
	want, enabled := p.cfgForWorkspace(e.root, e.language)
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.reconfiguring {
		e.reconfigureAgain = true
		return false
	}
	if !needsRestartLocked(e, want, enabled) {
		return false
	}
	e.reconfiguring = true
	go p.restartWithConfig(e, want, enabled)
	return true
}

// needsRestartLocked reports whether e must be restarted to run want. Nothing
// moved, or only the pool's own knobs (idle timeout, server cap): those are
// adopted in place, since the running server is what it should be. A disabled
// language whose server is already stopped needs nothing either — an acquire
// refuses it. Caller holds p.mu.
func needsRestartLocked(e *poolEntry, want config.LSPConfig, enabled bool) bool {
	if enabled && !serverChange(e.lspCfg, want) {
		e.lspCfg = want
		return false
	}
	return enabled || e.state != poolHibernated
}

// serverChange reports whether moving from a to b changes the server process
// itself. The idle timeout and the server cap are the pool's own knobs, read
// from the entry's config on every check, and the root markers only decide
// where a server is started — a change to those alone needs no restart.
func serverChange(a, b config.LSPConfig) bool {
	for _, c := range []*config.LSPConfig{&a, &b} {
		c.IdleTimeout, c.MaxWorkspaces = config.Duration{}, 0
		c.RootMarkers, c.WeakRootMarkers = nil, nil
	}
	return !reflect.DeepEqual(a, b)
}

// restartWithConfig is one entry's in-place restart: hibernate it once no call
// holds it, swap in a supervisor for cfg, and wake it when a session holds it.
func (p *workspacePool) restartWithConfig(e *poolEntry, cfg config.LSPConfig, enabled bool) {
	for {
		p.restartOnce(e, cfg, enabled)
		var again bool
		if cfg, enabled, again = p.nextRestart(e); !again {
			return
		}
	}
}

// nextRestart ends a restart round. When a reconfigure arrived during it, the
// config is resolved afresh and, if the server must change again, the round
// repeats with it; otherwise e.reconfiguring is cleared in the same p.mu
// section that read the request, so no later request is lost.
func (p *workspacePool) nextRestart(e *poolEntry) (config.LSPConfig, bool, bool) {
	for {
		p.mu.Lock()
		if !e.reconfigureAgain || p.closed || p.entries[poolKey{e.root, e.language}] != e {
			e.reconfigureAgain = false
			e.reconfiguring = false
			p.mu.Unlock()
			return config.LSPConfig{}, false, false
		}
		e.reconfigureAgain = false
		p.mu.Unlock()
		if p.projectConfigInvalid(e.root) {
			continue // re-checks the flag; ends the round unless a request came in
		}
		want, enabled := p.cfgForWorkspace(e.root, e.language)
		p.mu.Lock()
		restart := needsRestartLocked(e, want, enabled)
		p.mu.Unlock()
		if restart {
			return want, enabled, true
		}
	}
}

// restartOnce is one restart round: hibernate e once no call holds it, swap in
// a supervisor for cfg, and wake it when a session holds it.
func (p *workspacePool) restartOnce(e *poolEntry, cfg config.LSPConfig, enabled bool) {
	var env []string
	var goWorkOff string
	if enabled {
		env, goWorkOff = goLSPEnv(e.language, e.root, cfg, envFor(cfg))
	}
	if err := p.hibernateWhenIdle(e); err != nil {
		slog.Warn("pool: could not restart a language server for its new config", "root", e.root, "language", e.language, "err", err)
		return
	}
	// p.mu is held from here (hibernateWhenIdle returns with it).
	if p.closed || p.entries[poolKey{e.root, e.language}] != e || !enabled {
		p.mu.Unlock()
		if !enabled {
			slog.Info("pool: language disabled by config — its server stays stopped", "root", e.root, "language", e.language)
		}
		return
	}
	e.lspCfg = cfg
	e.goWorkOff = goWorkOff
	e.sup = p.newEntrySupervisor(e, cfg, env)
	e.diagDowngraded = false // a new server: re-negotiate diagnostics from config
	if e.cache != nil {
		e.cache.Clear()
	}
	held := e.refs > 0
	var readyCh <-chan error
	var err error
	if held {
		readyCh, err = p.wakeLocked(e)
	}
	p.mu.Unlock()
	slog.Info("pool: restarted a language server for its new config", "root", e.root, "language", e.language, "woken", held && err == nil)
	if err != nil {
		slog.Warn("pool: waking a reconfigured language server failed; the next acquire retries", "root", e.root, "language", e.language, "err", err)
		return
	}
	if held {
		// The wake does not rebuild the file watcher hibernation dropped; it is
		// attached here, after the unlock (attachWatcher, PLAN-536).
		p.attachWatcher(e)
	}
	if readyCh != nil {
		go p.reapOnLateStartFailure(e, readyCh)
	}
}

// hibernateWhenIdle hibernates e, waiting for the calls holding it to end —
// however long that takes: giving up would leave the old config running with
// nothing left to apply the new one. It says so every reconfigureWarnEvery, and
// stops when the entry is torn down or the daemon shuts down. On success it
// returns with p.mu HELD and e hibernated.
func (p *workspacePool) hibernateWhenIdle(e *poolEntry) error {
	nextWarn := time.Now().Add(reconfigureWarnEvery)
	ctx := p.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		p.hibernateEntry(e)
		p.mu.Lock()
		if e.state == poolHibernated {
			return nil
		}
		gone := p.closed || p.entries[poolKey{e.root, e.language}] != e
		p.mu.Unlock()
		if gone {
			return errors.New("the server was torn down meanwhile")
		}
		if !time.Now().Before(nextWarn) {
			slog.Warn("pool: a config change is waiting for calls in progress to end before restarting the server", "root", e.root, "language", e.language)
			nextWarn = time.Now().Add(reconfigureWarnEvery)
		}
		select {
		case <-ctx.Done():
			return errors.New("the daemon is shutting down")
		case <-time.After(reconfigurePoll):
		}
	}
}

// newEntrySupervisor builds the supervisor for e's language server under cfg:
// the OnStart handshake, and the workspace as its working directory (skipped
// when root is not an existing directory, so a deleted root degrades to the
// daemon's cwd rather than failing the spawn).
func (p *workspacePool) newEntrySupervisor(e *poolEntry, cfg config.LSPConfig, env []string) *lsp.Supervisor {
	return lsp.NewSupervisor(cfg.Command, argsFor(e.language, e.root, cfg), env, lsp.SupervisorOptions{
		OnStart: p.poolOnStart(e, protocol.FileURI(e.root), cfg),
		Dir:     existingDir(e.root),
	})
}

// globalConfigReconciler is the daemon's reaction to a global config change:
// reconfigure the shared pools that reload live — topology, and the language
// servers whose [lsp.<lang>] config changed — and warn when what changed still
// needs a whole-daemon restart (cache, log format, account authority).
func globalConfigReconciler(store *config.Store, topo *topologyPool, pool *workspacePool) func(config.Config) {
	return func(c config.Config) {
		topo.Reconcile(c.Topology)
		pool.applyGlobalLSP(c)
		if store.RestartNeeded() {
			slog.Warn("daemon: config change requires a restart to take effect (log format, cache, or account authority)")
		}
	}
}
