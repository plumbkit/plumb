package cli

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// This file holds the pool entry's teardown lifecycle: idle-grace reaping,
// first-start failure eviction (both the fast and slow-outcome paths), and the
// shutdown handshake shared by a single entry's teardown and daemon-wide
// pool.close(). Pool state, the acquire/release/pin paths, and the
// poolLifecycle type live in pool.go.

// reapEntry tears down an entry whose grace window elapsed, but only if it is
// still the mapped entry for root and still has no pins (a pin during the grace
// window cancels the timer, but the callback may already be running). Teardown
// happens outside p.mu — closeEntry performs a bounded LSP shutdown handshake we
// must not hold the pool lock across.
func (p *workspacePool) reapEntry(e *poolEntry) {
	key := poolKey{e.root, e.language}
	p.mu.Lock()
	cur, ok := p.entries[key]
	if !ok || cur != e || e.refs > 0 {
		p.mu.Unlock()
		return
	}
	delete(p.entries, key)
	p.mu.Unlock()
	slog.Info("pool: tearing down idle LS", "root", e.root, "language", e.language)
	ctx, cancel := context.WithTimeout(context.Background(), poolCloseGrace)
	defer cancel()
	e.closeOnce.Do(func() { closeEntry(ctx, e) })
}

// awaitReady waits up to startGrace for a freshly started entry to become
// ready. A first-start failure (e.g. a missing binary, which the supervisor
// will not retry) removes the entry so a later call re-spawns, and surfaces the
// error so attachWorkspace degrades to LanguageNone. On grace or request-context
// expiry the not-yet-ready entry is returned and the supervisor keeps warming.
//
// The failure path splits by timing. A FAST failure (spawn ENOENT, an OnStart
// that errors inside the grace) is observed here on readyCh and evicted inline.
// A SLOW failure — an OnStart that errors AFTER the grace, once we have already
// returned the not-yet-ready entry — would otherwise leave nobody reading
// readyCh, so removeFailed never runs and the dead entry (proxy.get() == nil)
// is reused forever with no self-heal. Whenever we bail before observing the
// outcome (grace or ctx expiry) we therefore hand readyCh to a drain goroutine
// that evicts the entry if the late outcome is an error. The supervisor
// guarantees exactly one send on readyCh over its lifetime, and this drain is
// its sole remaining reader, so the goroutine always terminates and never
// double-reads (a success delivers nil and the drain simply exits).
func (p *workspacePool) awaitReady(ctx context.Context, e *poolEntry, readyCh <-chan error) (*poolEntry, error) {
	grace := p.startGrace
	if grace <= 0 {
		grace = firstStartGrace
	}
	select {
	case startErr := <-readyCh:
		if startErr != nil {
			p.removeFailed(e)
			return nil, fmt.Errorf("starting %s for %s: %w", e.language, e.root, startErr)
		}
		return e, nil
	case <-time.After(grace):
		go p.reapOnLateStartFailure(e, readyCh)
		return e, nil
	case <-ctx.Done():
		go p.reapOnLateStartFailure(e, readyCh)
		return e, nil
	}
}

// reapOnLateStartFailure drains the first-start outcome for an entry awaitReady
// stopped waiting on (grace or request-ctx expiry), and evicts the entry if
// that outcome is a failure. It is the self-heal for a first start that fails
// SLOWLY: without it the supervisor's single readyCh send has no reader once
// awaitReady returns, so a dead entry lingers in the pool and is reused on every
// later acquire. A nil outcome (the server became ready after the grace) is the
// common case and is a no-op. removeFailed is idempotent (map-identity guard +
// closeOnce), so racing a concurrent reap/close is safe. No retry is started:
// eviction only lets the NEXT explicit acquire attempt a fresh start.
func (p *workspacePool) reapOnLateStartFailure(e *poolEntry, readyCh <-chan error) {
	startErr, ok := <-readyCh
	if !ok || startErr == nil {
		return
	}
	slog.Warn("pool: first start failed after grace — evicting dead entry so the next acquire retries",
		"root", e.root, "language", e.language, "err", startErr)
	p.removeFailed(e)
}

// removeFailed deletes a dead entry — one whose first start failed and which the
// supervisor will not retry — from the pool so a later acquire re-spawns, then
// tears down its supervisor and cache. The identity check guards against
// deleting a different entry a concurrent caller may have inserted for the same
// root.
func (p *workspacePool) removeFailed(e *poolEntry) {
	key := poolKey{e.root, e.language}
	p.mu.Lock()
	if cur, ok := p.entries[key]; ok && cur == e {
		delete(p.entries, key)
	}
	p.mu.Unlock()
	// closeOnce guards against the rare race where a parallel reapEntry for the
	// same entry already started teardown (a pin-then-immediate-fail sequence).
	e.closeOnce.Do(func() {
		if e.sup != nil {
			e.sup.Stop()
		}
		if e.watcher != nil {
			e.watcher.Stop()
		}
		if e.cache != nil {
			e.cache.Close()
		}
	})
}

// close shuts down all LS processes. Safe to call from multiple goroutines
// but intended to be called once at daemon shutdown. Entries are torn down
// concurrently under a bounded deadline so one slow language server cannot
// stall the others or daemon exit. Pending grace timers are cancelled so idle
// entries are shut down immediately rather than after their grace window.
func (p *workspacePool) close() {
	p.mu.Lock()
	// Snapshot entries and cancel any pending grace timers while holding the lock.
	entries := make([]*poolEntry, 0, len(p.entries))
	for _, e := range p.entries {
		if e.graceTimer != nil {
			e.graceTimer.Stop()
			e.graceTimer = nil
		}
		entries = append(entries, e)
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), poolCloseGrace)
	defer cancel()

	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// closeOnce ensures a reapEntry goroutine already running for this
			// entry (race between grace timer firing and daemon shutdown) does
			// not double-close the supervisor and cache.
			e.closeOnce.Do(func() { closeEntry(ctx, e) })
		}()
	}
	// Bound the aggregate wait: the entries close concurrently, so the whole set
	// finishes within one poolCloseGrace handshake + one supStopGrace supervisor
	// stop even when several are torn down at once. A wedged entry is abandoned
	// (and logged) rather than blocking daemon exit until the watchdog forces it —
	// the process is exiting, so the leaked goroutine and its child are reclaimed
	// by exit. closeEntry stays unbounded when called from the idle-teardown path
	// (reapEntry), where the daemon keeps running and must not leak a server.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	waitWithTimeout(done, poolCloseGrace+supStopGrace, "lsp pool")
}

// closeEntry shuts down a single pool entry: a best-effort bounded LSP
// Shutdown/Exit handshake, then sup.Stop() to kill the process regardless of
// whether the handshake completed. The handshake is politeness; sup.Stop()
// (which cancels the supervisor's exec.CommandContext, killing the process) is
// the real guarantee the language server dies.
func closeEntry(ctx context.Context, e *poolEntry) {
	if c := e.proxy.get(); c != nil {
		_ = c.Shutdown(ctx)
		_ = c.Exit(ctx)
	}
	if e.sup != nil {
		e.sup.Stop()
	}
	if e.watcher != nil {
		e.watcher.Stop()
	}
	if e.cache != nil {
		e.cache.Close()
	}
}
