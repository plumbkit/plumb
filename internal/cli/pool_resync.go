package cli

import (
	"log/slog"
	"time"
)

// PLAN-538: a language server is resynced when its file watcher loses events,
// instead of the log recommending a whole-daemon restart (which re-warmed every
// project). LSP has no portable "re-read the workspace" notification, so the
// resync is the in-place restart PLAN-539 built (restartWithConfig with the
// entry's own config): it waits for the calls in progress, starts a fresh server
// that reads the tree from disk, and keeps the entry's leases and slot. Only the
// entry whose watcher lost events is touched.
//
// A burst of losses — an overflowing buffer reports Lost repeatedly — collapses
// into one resync after resyncQuiet, and an entry is resynced at most once per
// resyncMinInterval, so a storm costs one restart, not hundreds. A watcher that
// fails is recreated with backoff, and the server resynced once it is back.

const (
	lspResyncQuiet       = 30 * time.Second
	lspResyncMinInterval = 10 * time.Minute
	lspWatcherRetryBase  = 5 * time.Second
	lspWatcherRetryMax   = 5 * time.Minute
)

// resyncDelays returns the pool's resync timings, overridable in tests.
func (p *workspacePool) resyncDelays() (quiet, minInterval time.Duration) {
	quiet, minInterval = lspResyncQuiet, lspResyncMinInterval
	if p.resyncQuiet > 0 {
		quiet = p.resyncQuiet
	}
	if p.resyncMinInterval > 0 {
		minInterval = p.resyncMinInterval
	}
	return quiet, minInterval
}

// onWatcherLost schedules a resync of e after its watcher lost events. A resync
// already pending absorbs this one.
func (p *workspacePool) onWatcherLost(e *poolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.entries[poolKey{e.root, e.language}] != e || e.resyncTimer != nil {
		return
	}
	quiet, minInterval := p.resyncDelays()
	delay := quiet
	if !e.lastResync.IsZero() {
		if wait := time.Until(e.lastResync.Add(minInterval)); wait > delay {
			delay = wait
		}
	}
	e.resyncTimer = time.AfterFunc(delay, func() { p.resyncEntry(e) })
	slog.Warn("lsp: file watcher lost events — resyncing this language server", "root", e.root, "language", e.language, "in", delay.Round(time.Second))
}

// resyncEntry restarts e in place so its server re-reads the tree. A hibernated
// server re-reads it when it wakes, and a restart already running resyncs it.
func (p *workspacePool) resyncEntry(e *poolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.resyncTimer = nil
	if p.closed || p.entries[poolKey{e.root, e.language}] != e || e.sup == nil || !e.state.running() {
		return
	}
	e.lastResync = time.Now()
	p.resyncs++
	p.lastResynced = poolKey{e.root, e.language}
	if e.reconfiguring {
		return
	}
	e.reconfiguring = true
	go p.restartWithConfig(e, e.lspCfg, true)
}

// onWatcherFailed starts recreating e's watcher, unless that is already under
// way.
func (p *workspacePool) onWatcherFailed(e *poolEntry) {
	p.mu.Lock()
	if e.watcherRetrying {
		p.mu.Unlock()
		return
	}
	e.watcherRetrying = true
	p.mu.Unlock()
	go p.recreateWatcher(e)
}

// recreateWatcher replaces e's failed watcher after a backoff that doubles with
// each failure (capped), then resyncs the server for what it missed. It gives
// up when the entry is gone, stopped (a wake attaches a fresh watcher) or the
// pool closes.
func (p *workspacePool) recreateWatcher(e *poolEntry) {
	defer func() {
		p.mu.Lock()
		e.watcherRetrying = false
		p.mu.Unlock()
	}()
	p.mu.Lock()
	delay := lspWatcherRetryBase << min(e.watcherFailures, 6)
	e.watcherFailures++
	p.mu.Unlock()
	if p.watcherRetryDelay > 0 {
		delay = p.watcherRetryDelay
	}
	select {
	case <-p.shutdownCtx().Done():
		return
	case <-time.After(min(delay, lspWatcherRetryMax)):
	}
	p.mu.Lock()
	if p.closed || p.entries[poolKey{e.root, e.language}] != e || !e.state.running() {
		p.mu.Unlock()
		return
	}
	old := e.watcher
	e.watcher = nil
	p.mu.Unlock()
	if old != nil {
		old.Stop()
	}
	p.attachWatcher(e)
	slog.Info("lsp: file watcher recreated", "root", e.root, "language", e.language)
	p.onWatcherLost(e) // changes made while it was down were not seen
}
