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

// reconfigureWait bounds how long a restart waits for the calls in progress on
// an entry to finish; reconfigurePoll is how often it retries the hibernation.
const (
	reconfigureWait = 30 * time.Second
	reconfigurePoll = 20 * time.Millisecond
)

// applyGlobalLSP adopts a new global config's [lsp.*] blocks and restarts the
// entries whose effective config they change. A language enabled live with
// `plumb enable-lsp` stays enabled. Returns how many entries it restarts.
func (p *workspacePool) applyGlobalLSP(cfg config.Config) int {
	next := make(map[string]config.LSPConfig, len(cfg.LSP))
	maps.Copy(next, cfg.LSP)
	p.mu.Lock()
	for name := range p.liveEnabled {
		if l, ok := next[name]; ok {
			l.Enabled = true
			next[name] = l
		}
	}
	p.mu.Unlock()

	p.langsMu.Lock()
	if reflect.DeepEqual(p.baseConfig.LSP, next) {
		p.langsMu.Unlock()
		return 0
	}
	p.baseConfig.LSP = next
	scoped := cfg
	scoped.LSP = next
	p.langs = activeLanguages(scoped)
	p.langsMu.Unlock()
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

// reconfigureEntries restarts, in the background, every selected entry whose
// effective config now differs from the one it was started with. An entry whose
// language the new config disables is hibernated and stays so (an acquire now
// refuses it). Returns how many restarts it started.
func (p *workspacePool) reconfigureEntries(selected func(*poolEntry) bool) int {
	p.mu.Lock()
	var cands []*poolEntry
	for _, e := range p.entries {
		if selected(e) && !e.reconfiguring {
			cands = append(cands, e)
		}
	}
	p.mu.Unlock()
	n := 0
	for _, e := range cands {
		want, enabled := p.cfgForWorkspace(e.root, e.language)
		if enabled && reflect.DeepEqual(want, e.lspCfg) {
			continue
		}
		if enabled && !serverChange(e.lspCfg, want) {
			// Only the pool's own knobs moved (idle timeout, server cap): adopt
			// them in place; the running server is exactly what it should be.
			p.mu.Lock()
			e.lspCfg = want
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		if e.reconfiguring {
			p.mu.Unlock()
			continue
		}
		e.reconfiguring = true
		p.mu.Unlock()
		n++
		go p.restartWithConfig(e, want, enabled)
	}
	return n
}

// serverChange reports whether moving from a to b changes the server process
// itself. The idle timeout and the server cap are the pool's own knobs, read
// from the entry's config on every check, so a change to them alone needs no
// restart.
func serverChange(a, b config.LSPConfig) bool {
	a.IdleTimeout, b.IdleTimeout = config.Duration{}, config.Duration{}
	a.MaxWorkspaces, b.MaxWorkspaces = 0, 0
	return !reflect.DeepEqual(a, b)
}

// restartWithConfig is one entry's in-place restart: hibernate it once no call
// holds it, swap in a supervisor for cfg, and wake it when a session holds it.
func (p *workspacePool) restartWithConfig(e *poolEntry, cfg config.LSPConfig, enabled bool) {
	defer func() {
		p.mu.Lock()
		e.reconfiguring = false
		p.mu.Unlock()
	}()
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
	if p.entries[poolKey{e.root, e.language}] != e || !enabled {
		p.mu.Unlock()
		slog.Info("pool: language disabled by config — its server stays stopped", "root", e.root, "language", e.language)
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
	if readyCh != nil {
		go p.reapOnLateStartFailure(e, readyCh)
	}
}

// hibernateWhenIdle hibernates e, waiting for the calls holding it to end
// (bounded by reconfigureWait, ended early by daemon shutdown). On success it
// returns with p.mu HELD and e hibernated.
func (p *workspacePool) hibernateWhenIdle(e *poolEntry) error {
	deadline := time.Now().Add(reconfigureWait)
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
		p.mu.Unlock()
		if !time.Now().Before(deadline) {
			return errors.New("calls still in progress after the wait")
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
// needs a whole-daemon restart (cache, log format, log file, account authority).
func globalConfigReconciler(store *config.Store, topo *topologyPool, pool *workspacePool) func(config.Config) {
	return func(c config.Config) {
		topo.Reconcile(c.Topology)
		pool.applyGlobalLSP(c)
		if store.RestartNeeded() {
			slog.Warn("daemon: config change requires a restart to take effect (log format, log file, cache, or account authority)")
		}
	}
}
