package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/monitor"
)

// This file holds the pool's dynamic-resource-manager concern: idle hibernation
// of heavyweight language servers, LRU eviction, the janitor goroutine, live
// PID/RSS reporting, and per-root server-state cache pruning. The acquire/release/pin
// paths and pool state live in pool.go; entry teardown (idle-grace reaping,
// first-start failure eviction, daemon-wide close) lives in pool_lifecycle.go;
// the poolLifecycle type and poolEntry.state are declared in pool.go, and the
// activity timestamp on clientProxy.lastUsed (proxy.go).

// lspStatusReport renders one tab-separated line per pooled language server —
// language, root, lifecycle state, PID, RSS bytes, idle seconds, and the
// resolved diagnostics mode (`diag=<mode>`) — for the `plumb debug lsp` admin
// command. Empty PID/RSS fields mean the process is not running (hibernated or
// warming); an empty diag mode (`diag=`) means it is not yet resolved. The map
// is snapshotted under p.mu; RSS sampling (which may shell out on macOS) runs
// after the lock is released.
func (p *workspacePool) lspStatusReport() string {
	type row struct {
		lang, root, state string
		pid               int
		lastUsed          int64
		diag              string
		refs              int
		inflight          int64
		overCap           bool
	}
	p.mu.Lock()
	rows := make([]row, 0, len(p.entries))
	for k, e := range p.entries {
		pid := 0
		if e.sup != nil {
			pid = e.sup.PID()
		}
		rows = append(rows, row{
			k.language, k.root, e.state.String(), pid, e.proxy.lastUsed.Load(), e.diagMode,
			e.refs, e.proxy.inflight.Load(), p.overCap[k.language],
		})
	}
	p.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].lang != rows[j].lang {
			return rows[i].lang < rows[j].lang
		}
		return rows[i].root < rows[j].root
	})
	now := time.Now().UnixNano()
	var b strings.Builder
	for _, r := range rows {
		pidStr, rssStr := "", ""
		if r.pid > 0 {
			pidStr = strconv.Itoa(r.pid)
			if rss, ok := monitor.ProcessRSS(r.pid); ok {
				rssStr = strconv.FormatUint(rss, 10)
			}
		}
		idle := (now - r.lastUsed) / int64(time.Second)
		over := 0
		if r.overCap {
			over = 1
		}
		// Fields 8-10 (refs, in-flight calls, language over its cap) were added
		// by PLAN-534 after the first seven; readers accept seven or more.
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%d\tdiag=%s\t%d\t%d\t%d\n", r.lang, r.root, r.state, pidStr, rssStr, idle, r.diag, r.refs, r.inflight, over)
	}
	return b.String()
}

// startJanitor launches the hibernation janitor on ctx (the daemon-lifetime
// context). It exits when ctx is cancelled. Call once, from the daemon — not
// from one-shot CLI pools, which would leak the goroutine on a Background ctx.
func (p *workspacePool) startJanitor(ctx context.Context) {
	go p.janitor(ctx)
}

func (p *workspacePool) janitor(ctx context.Context) {
	idleTicker := time.NewTicker(janitorInterval)
	defer idleTicker.Stop()
	pruneTicker := time.NewTicker(cachePruneInterval)
	defer pruneTicker.Stop()
	p.pruneServerStateCaches() // reclaim last run's stale dirs at startup
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-idleTicker.C:
			p.hibernateIdle()
			p.trimLongHibernated(now)
			p.sampleBudget(now)
		case <-pruneTicker.C:
			p.pruneServerStateCaches()
		}
	}
}

// pruneServerStateCaches prunes the per-root state directory of every language
// in serverStateDirs. These stores are large — an Eclipse workspace runs ~50 MB
// per project, and an IntelliJ system path backs a 1.3 GB server — so a language
// added to serverStateDirs without pruning would accumulate one directory per
// project ever opened, forever.
func (p *workspacePool) pruneServerStateCaches() {
	for language, spec := range serverStateDirs {
		p.pruneStateDir(language, spec)
	}
}

// pruneStateDir removes one language's state directories that are not backing a
// currently-pooled workspace and whose directory mtime is older than
// serverStateMaxAge. A live entry's dir is always kept (even when the entry is
// hibernated — waking reuses it). Best-effort: a missing base dir or a failed
// removal is logged, never fatal.
func (p *workspacePool) pruneStateDir(language string, spec stateDirSpec) {
	base := filepath.Join(config.CacheDir(), spec.subdir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return // nothing cached for this language yet
	}
	inUse := p.inUseStateDirs(language)
	cutoff := time.Now().Add(-serverStateMaxAge)
	for _, ent := range entries {
		if !ent.IsDir() || inUse[ent.Name()] {
			continue
		}
		info, err := ent.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		full := filepath.Join(base, ent.Name())
		if err := os.RemoveAll(full); err != nil {
			slog.Warn("pool: server state prune failed", "language", language, "dir", full, "err", err)
			continue
		}
		slog.Info("pool: pruned stale server state", "language", language, "dir", full,
			"age_days", int(time.Since(info.ModTime()).Hours()/24))
	}
}

// inUseStateDirs returns the set of state-directory names backing a pooled
// workspace of the given language, so pruning never deletes an in-use store.
func (p *workspacePool) inUseStateDirs(language string) map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]bool)
	for k := range p.entries {
		if k.language == language {
			out[filepath.Base(serverStateDir(language, k.root))] = true
		}
	}
	return out
}

// hibernateIdle hibernates every running entry whose language sets a positive
// idle_timeout and whose last tool call is older than it. Candidates are
// selected under p.mu; the blocking teardown runs outside the lock.
func (p *workspacePool) hibernateIdle() {
	now := time.Now().UnixNano()
	var victims []*poolEntry
	p.mu.Lock()
	for _, e := range p.entries {
		if !e.state.running() {
			continue
		}
		cfg := e.lspCfg
		if cfg.IdleTimeout.Duration <= 0 {
			continue
		}
		if now-e.proxy.lastUsed.Load() > int64(cfg.IdleTimeout.Duration) {
			victims = append(victims, e)
		}
	}
	p.mu.Unlock()
	for _, e := range victims {
		p.hibernateEntry(e)
	}
}

// overBudgetVictimLocked returns the least-recently-used running entry of
// language when starting one more would exceed max running servers, or nil when
// max is unlimited (<=0) or the budget is not yet reached. Caller holds p.mu.
func (p *workspacePool) overBudgetVictimLocked(language string, maxRunning int, exclude *poolEntry) *poolEntry {
	if maxRunning <= 0 {
		return nil
	}
	var running []*poolEntry
	for k, e := range p.entries {
		if k.language == language && e != exclude && e.state.running() {
			running = append(running, e)
		}
	}
	if len(running) < maxRunning {
		return nil
	}
	// A server with a call in progress is never the victim; when every one is
	// busy there is none, and the new server starts over budget. Among the idle
	// ones an unpinned server (no session leases it) goes first, then a pinned
	// one, least recently used within each tier (PLAN-534).
	var victim *poolEntry
	for _, e := range running {
		if e.proxy.inflight.Load() > 0 {
			continue
		}
		if victim == nil || victimBefore(e, victim) {
			victim = e
		}
	}
	return victim
}

// victimBefore orders eviction candidates: unpinned before pinned, then least
// recently used.
func victimBefore(a, b *poolEntry) bool {
	if (a.refs == 0) != (b.refs == 0) {
		return a.refs == 0
	}
	return a.proxy.lastUsed.Load() < b.proxy.lastUsed.Load()
}

// hibernateEntry stops an active entry's language-server process to reclaim its
// memory, keeping the poolEntry, its warm cache, and its map slot so the next
// acquire restarts it (wakeLocked). Distinct from reapEntry, which deletes the
// entry entirely. The proxy is cleared and state set under p.mu BEFORE the
// out-of-lock teardown, so a concurrent route() sees the server as not-ready and
// triggers a restart rather than calling into a dying connection. sup.Stop()
// blocks on its goroutine, so it must never run under p.mu (mirrors reapEntry).
// closeOnce is deliberately NOT consumed — a later reapEntry must still be able
// to fully tear the entry down.
func (p *workspacePool) hibernateEntry(e *poolEntry) {
	p.mu.Lock()
	teardown := p.beginHibernateLocked(e)
	p.mu.Unlock()
	if teardown != nil {
		teardown()
	}
}

// beginHibernateLocked is hibernateEntry's under-lock half: it moves a running
// entry with no call in progress to hibernating and returns the out-of-lock
// teardown that stops its process and finishes the transition, or nil when the
// entry is not hibernated. Admission (admitLocked) calls it so the slot frees in
// the same p.mu section that admits the newcomer. Caller holds p.mu.
func (p *workspacePool) beginHibernateLocked(e *poolEntry) func() {
	if !e.state.running() {
		return nil
	}
	// Clear first, then look for a call lease, restoring the client if one is
	// held: the mirror image of clientProxy.begin, so a call either keeps the
	// server or sees it as not ready, never holds one being stopped.
	client := e.proxy.get()
	e.proxy.clear()
	if e.proxy.inflight.Load() > 0 {
		e.proxy.set(client)
		slog.Debug("pool: hibernation skipped — a call is in progress", "root", e.root, "language", e.language)
		return nil
	}
	e.state = poolHibernating
	sup := e.sup
	watcher := e.watcher
	e.watcher = nil
	return func() { p.finishHibernate(e, client, sup, watcher) }
}

// finishHibernate is the out-of-lock teardown beginHibernateLocked returns.
func (p *workspacePool) finishHibernate(e *poolEntry, client lsp.Client, sup *lsp.Supervisor, watcher *lspFSWatcher) {
	root, language := e.root, e.language
	slog.Info("pool: hibernating idle LS", "root", root, "language", language)
	ctx, cancel := context.WithTimeout(context.Background(), poolCloseGrace)
	if client != nil {
		_ = client.Shutdown(ctx)
		_ = client.Exit(ctx)
	}
	cancel()
	if sup != nil {
		sup.Stop()
	}
	if watcher != nil {
		watcher.Stop()
	}

	p.mu.Lock()
	if e.state == poolHibernating {
		e.state = poolHibernated
		e.hibernatedAt = time.Now()
	}
	p.mu.Unlock()
}

// wakeLocked restarts a hibernated entry's language server, reusing the same
// Supervisor (StartAsync re-runs the captured OnStart, re-publishing the adapter
// into the entry's proxy) and the same warm cache. The file watcher is not
// restartable; the caller rebuilds it after releasing p.mu (attachWatcher).
// Returns the first-start channel for awaitReady. Caller holds p.mu and must
// have verified e.state == poolHibernated.
func (p *workspacePool) wakeLocked(e *poolEntry) (<-chan error, error) {
	readyCh, err := e.sup.StartAsync(p.baseCtx)
	if err != nil {
		return nil, fmt.Errorf("waking %s for %s: %w", e.language, e.root, err)
	}
	// The file watcher is not rebuilt here: its initial walk must not run under
	// p.mu, so the caller attaches it after the unlock (attachWatcher).
	e.state = poolWaking
	e.startedAt = time.Now()
	e.hibernatedAt = time.Time{}
	e.trimmed = false
	e.proxy.touch()
	return readyCh, nil
}
