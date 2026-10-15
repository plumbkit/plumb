package cli

import (
	"context"
	"log/slog"
	"time"
)

// This file holds the pool's lease accounting (PLAN-533). The pool owns every
// entry; what keeps one alive is its leases: SESSION leases (refs: a pinned
// primary, or a borrow by a connection routing to it) and CALL leases
// (clientProxy.inflight: a call in progress). Hibernation needs no call lease;
// reaping needs neither, and then waits idleGrace. Acquire, wake and the
// lifecycle states live in pool.go; teardown in pool_lifecycle.go and
// pool_hibernate.go.

// acquireCall returns (or starts) the entry for (root, language) holding a CALL
// lease on it: the entry is neither hibernated nor reaped until the returned
// done is called, which the caller does when its call ends (done is idempotent).
// The lease is taken under the pool lock, at the same moment the entry is found
// or created, so no teardown can slip in between the acquire and the call. done
// is nil exactly when err is non-nil.
func (p *workspacePool) acquireCall(ctx context.Context, root, language string) (*poolEntry, func(), error) {
	e, readyCh, err := p.startOrReuse(root, language, false, true)
	if err != nil {
		return nil, nil, err
	}
	done := e.proxy.endOnce()
	if readyCh == nil {
		return e, done, nil
	}
	ready, err := p.awaitReady(ctx, e, readyCh)
	if err != nil {
		done()
		return nil, nil, err
	}
	return ready, done, nil
}

// leaseLocked takes the leases an acquire asked for: a session lease (pin) and
// a call lease (call). Either cancels a pending idle teardown. Caller holds p.mu.
func (p *workspacePool) leaseLocked(e *poolEntry, pin, call bool) {
	if pin {
		p.pinLocked(e)
	}
	if call {
		e.proxy.inflight.Add(1)
		p.cancelGraceLocked(e)
	}
}

// pinLocked increments an entry's session refcount and cancels any pending
// idle-teardown timer (a re-pin during the grace window keeps the server
// alive). Caller must hold p.mu.
func (p *workspacePool) pinLocked(e *poolEntry) {
	e.refs++
	e.proxy.held.Store(true)
	p.cancelGraceLocked(e)
}

// cancelGraceLocked stops a pending idle teardown. Caller holds p.mu.
func (p *workspacePool) cancelGraceLocked(e *poolEntry) {
	if e.graceTimer != nil {
		e.graceTimer.Stop()
		e.graceTimer = nil
	}
}

// borrow takes a session lease on e for a session whose primary is elsewhere
// (routingProxy borrows each entry it routes to). It is keyed by the entry, not
// the (root, language) pair, so it can never land on a replacement entry; it
// reports false when e is no longer the pool's entry for its key.
func (p *workspacePool) borrow(e *poolEntry) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[poolKey{e.root, e.language}] != e {
		return false
	}
	p.pinLocked(e)
	return true
}

// unborrow returns a lease taken by borrow; a no-op when e has left the pool.
func (p *workspacePool) unborrow(e *poolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[poolKey{e.root, e.language}] != e {
		return
	}
	p.unleaseLocked(e)
}

// scheduleIdleReap (re-)arms the idle teardown for an entry that has just lost
// its last call lease, when nothing else holds it, so the grace counts from the
// end of the last call. A timer already armed is replaced: it may have been
// armed by a release while this call was still running, and keeping it would
// reap the entry a grace after that release rather than after this call. The
// checks repeat under the lock because the call's end read the held mirror
// without it.
func (p *workspacePool) scheduleIdleReap(e *poolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[poolKey{e.root, e.language}] != e || e.refs > 0 || e.proxy.inflight.Load() > 0 {
		return
	}
	p.cancelGraceLocked(e)
	e.graceTimer = time.AfterFunc(p.idleGrace, func() { p.reapEntry(e) })
}

// release drops one pinned session reference on root (paired with an
// acquireLang(pin=true)). When the last reference goes, the entry is scheduled
// for teardown after idleGrace rather than torn down immediately, so a client
// reconnect or quick re-attach reuses the warm server. A no-op when root has no
// entry or no outstanding pins (defensive: a session that attached without LSP,
// LanguageNone, holds no pin and must not decrement a sibling's count).
func (p *workspacePool) release(root, language string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[poolKey{root, language}]
	if !ok {
		return
	}
	p.unleaseLocked(e)
}

// unleaseLocked drops one session lease. When the last goes, the entry is
// scheduled for teardown after idleGrace; a call still running then is safe,
// because the reap refuses an entry with a call lease and the call's end
// re-arms the timer. Caller holds p.mu.
func (p *workspacePool) unleaseLocked(e *poolEntry) {
	if e.refs <= 0 {
		return
	}
	e.refs--
	if e.refs > 0 {
		return
	}
	e.proxy.held.Store(false)
	if e.graceTimer != nil {
		e.graceTimer.Stop()
	}
	e.graceTimer = time.AfterFunc(p.idleGrace, func() { p.reapEntry(e) })
	slog.Info("pool: last lease released — scheduling idle teardown", "root", e.root, "language", e.language, "grace", p.idleGrace)
}

// markActive records that a starting or waking entry finished its handshake. It
// runs on the supervisor goroutine (OnStart), never under p.mu: StartAsync only
// launches that goroutine, so the acquire that holds p.mu cannot be waiting on
// it. A restart after a crash finds the entry already active and changes nothing;
// an entry hibernated meanwhile keeps its hibernation state.
func (p *workspacePool) markActive(e *poolEntry) {
	p.mu.Lock()
	if e.state == poolStarting || e.state == poolWaking {
		e.state = poolActive
	}
	p.mu.Unlock()
}
