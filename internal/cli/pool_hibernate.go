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
	"github.com/plumbkit/plumb/internal/monitor"
)

// This file holds the pool's dynamic-resource-manager concern: idle hibernation
// of heavyweight language servers, LRU eviction, the janitor goroutine, live
// PID/RSS reporting, and jdtls-data cache pruning. The acquire/release/teardown
// lifecycle and pool state live in pool.go; poolEntry.lastUsed/state and the
// poolLifecycle type are declared there.

// touch records that a tool call was routed to (root, language) just now, so the
// hibernation janitor and LRU eviction see the entry as recently active. Called
// from the routing proxy at the point a live client is handed to a tool — not
// from clientProxy.get(), which also fires for teardown and capability probes
// that must not reset the idle clock. A no-op for an unknown or hibernated
// entry. Concurrency-safe (atomic store; brief map lookup under p.mu).
func (p *workspacePool) touch(root, language string) {
	if e := p.lookup(root, language); e != nil {
		e.lastUsed.Store(time.Now().UnixNano())
	}
}

// lspStatusReport renders one tab-separated line per pooled language server —
// language, root, lifecycle state, PID, RSS bytes, idle seconds — for the
// `plumb debug lsp` admin command. Empty PID/RSS fields mean the process is not
// running (hibernated or warming). The map is snapshotted under p.mu; RSS
// sampling (which may shell out on macOS) runs after the lock is released.
func (p *workspacePool) lspStatusReport() string {
	type row struct {
		lang, root, state string
		pid               int
		lastUsed          int64
	}
	p.mu.Lock()
	rows := make([]row, 0, len(p.entries))
	for k, e := range p.entries {
		pid := 0
		if e.sup != nil {
			pid = e.sup.PID()
		}
		rows = append(rows, row{k.language, k.root, e.state.String(), pid, e.lastUsed.Load()})
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
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%d\n", r.lang, r.root, r.state, pidStr, rssStr, idle)
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
	p.pruneJdtlsCache() // reclaim last run's stale dirs at startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-idleTicker.C:
			p.hibernateIdle()
		case <-pruneTicker.C:
			p.pruneJdtlsCache()
		}
	}
}

// pruneJdtlsCache removes jdtls Eclipse-workspace data directories that are not
// backing a currently-pooled Java workspace and whose directory mtime is older
// than jdtlsCacheMaxAge. A live entry's dir is always kept (even when the entry
// is hibernated — waking reuses it). Best-effort: a missing base dir or a failed
// removal is logged, never fatal.
func (p *workspacePool) pruneJdtlsCache() {
	base := filepath.Join(config.CacheDir(), "jdtls-data")
	entries, err := os.ReadDir(base)
	if err != nil {
		return // no jdtls-data dir yet
	}
	inUse := p.inUseJdtlsDirs()
	cutoff := time.Now().Add(-jdtlsCacheMaxAge)
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
			slog.Warn("pool: jdtls-data prune failed", "dir", full, "err", err)
			continue
		}
		slog.Info("pool: pruned stale jdtls-data", "dir", full, "age_days", int(time.Since(info.ModTime()).Hours()/24))
	}
}

// inUseJdtlsDirs returns the set of jdtls-data directory names backing a pooled
// Java workspace, so pruning never deletes an in-use store.
func (p *workspacePool) inUseJdtlsDirs() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]bool)
	for k := range p.entries {
		if k.language == "java" {
			out[filepath.Base(jdtlsDataDir(k.root))] = true
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
		if e.state != poolActive {
			continue
		}
		cfg, ok := p.cfgFor(e.language)
		if !ok || cfg.IdleTimeout.Duration <= 0 {
			continue
		}
		if now-e.lastUsed.Load() > int64(cfg.IdleTimeout.Duration) {
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
func (p *workspacePool) overBudgetVictimLocked(language string, maxRunning int) *poolEntry {
	if maxRunning <= 0 {
		return nil
	}
	var running []*poolEntry
	for k, e := range p.entries {
		if k.language == language && e.state == poolActive {
			running = append(running, e)
		}
	}
	if len(running) < maxRunning {
		return nil
	}
	victim := running[0]
	for _, e := range running[1:] {
		if e.lastUsed.Load() < victim.lastUsed.Load() {
			victim = e
		}
	}
	return victim
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
	if e.state != poolActive {
		p.mu.Unlock()
		return
	}
	e.state = poolHibernating
	client := e.proxy.get()
	e.proxy.clear()
	sup := e.sup
	watcher := e.watcher
	e.watcher = nil
	root, language := e.root, e.language
	p.mu.Unlock()

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
	}
	p.mu.Unlock()
}

// wakeLocked restarts a hibernated entry's language server, reusing the same
// Supervisor (StartAsync re-runs the captured OnStart, re-publishing the adapter
// into the entry's proxy) and the same warm cache. The file watcher is not
// restartable, so it is rebuilt. Returns the first-start channel for awaitReady.
// Caller holds p.mu and must have verified e.state == poolHibernated.
func (p *workspacePool) wakeLocked(e *poolEntry) (<-chan error, error) {
	readyCh, err := e.sup.StartAsync(p.baseCtx)
	if err != nil {
		return nil, fmt.Errorf("waking %s for %s: %w", e.language, e.root, err)
	}
	if watcher, err := newLSPFSWatcher(e.root, e.proxy); err != nil {
		slog.Warn("lsp: file watcher unavailable on wake", "root", e.root, "language", e.language, "err", err)
	} else {
		e.watcher = watcher
		watcher.Start()
	}
	e.state = poolActive
	e.lastUsed.Store(time.Now().UnixNano())
	return readyCh, nil
}
