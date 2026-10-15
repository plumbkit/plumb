package cli

// This file holds the routing proxy's session leases on entries it routes to
// beyond its primary (PLAN-533). The primary is pinned by attach / re-pin; every
// other entry a connection calls into — a secondary language under its root, a
// nested root such as plumb-ops routing into plumb, a fan-out target — is
// BORROWED: the connection takes a lease the first time it reaches the entry and
// returns it when the connection ends or deliberately switches workspace. The
// pool owns the entry and reaps it idleGrace after the last lease and the last
// call are gone, instead of keeping it until daemon shutdown.

// borrowEntry takes this connection's lease on e, once per entry. The primary is
// skipped: its pin already holds it. leaseMu serialises borrow against release so
// a release never returns a lease that was not taken; it is always taken before
// the pool mutex, and the pool never calls back into the proxy.
func (r *routingProxy) borrowEntry(e *poolEntry) {
	r.mu.RLock()
	primary := e.root == r.primaryRoot && e.language == r.primaryLang
	r.mu.RUnlock()
	if primary {
		return
	}
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	if _, ok := r.borrowed[e]; ok {
		return
	}
	if !r.pool.borrow(e) {
		return // the entry left the pool; the next call acquires a fresh one
	}
	if r.borrowed == nil {
		r.borrowed = make(map[*poolEntry]struct{}, 1)
	}
	r.borrowed[e] = struct{}{}
}

// releaseBorrowed returns every lease this connection borrowed. Called when the
// connection ends and on a deliberate workspace switch (resetPrimary), which
// starts with a clean set the way activated does.
func (r *routingProxy) releaseBorrowed() {
	r.leaseMu.Lock()
	borrowed := r.borrowed
	r.borrowed = nil
	r.leaseMu.Unlock()
	for e := range borrowed {
		r.pool.unborrow(e)
	}
}
