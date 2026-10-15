package cli

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
)

// PLAN-535: the per-root topology, memory and collab stores are opened lazily
// and, before this, lived until the daemon stopped — a long-lived daemon held
// every project's databases open, including roots from deleted scratch
// directories. Each pool now records when a root was last used, and a janitor
// drains a root that no connection holds and that has been idle for
// storeIdleGrace: it claims the root's flight (so an acquire racing the drain
// waits for it and then re-opens), unpublishes the store, and closes it — which
// stops and joins its background work (topology's watcher and indexer, memory's
// reindex) — all outside the pool mutex.
//
// "Held" is a live connection on the root: its workspace or one of its logical
// agents' (connRegistry.heldRoots), or a project-config watch reference. A held
// root is re-stamped on every pass, so its grace starts when it is released,
// not at its last pool lookup — an attached session reads its cached handle
// without one. A root reached only by a cross-project note or a health probe is
// kept alive by use alone. The collab pool's daemon-level store is never
// drained.
//
// A caller that kept a store across a drain gets a clean error from the closed
// database, never a crash; the topology session handle re-acquires (see
// topologyStoreLive).

// storeIdleGrace is how long an unheld root's stores stay open after their last
// use; storeDrainEvery is how often the janitor looks.
const (
	storeIdleGrace  = 10 * time.Minute
	storeDrainEvery = time.Minute
	// storeCloseGrace bounds how long a pass waits on one store's close: a
	// wedged indexer must not stall every other drain (StopAll's same rule).
	// The flight stays claimed until the close returns, so that root's next
	// open still waits for it.
	storeCloseGrace = 2 * time.Second
)

// drainSet is one pool's view for drainIdle: its mutex, its published stores,
// their use stamps and its open flights. forget drops any other per-root state
// the pool keeps (topology's per-root config).
type drainSet[S any] struct {
	mu     *sync.Mutex
	stores *map[string]S
	used   *map[string]time.Time
	flight *keyedFlight
	forget func(root string)
	close  func(root string, s S)
}

// drainIdle closes every published store that no connection holds and that has
// gone storeIdleGrace unused as of now. The candidates are taken under the
// pool mutex; held runs outside it; each drain re-checks under the mutex (a use
// since the scan spares the root), claims the root's flight so an open racing
// it waits and then re-opens, unpublishes the store, and closes it outside the
// mutex — ending the flight even if the close panics. Returns how many it closed.
func drainIdle[S any](d drainSet[S], now time.Time, held func(root string) bool) int {
	d.mu.Lock()
	var cands []string
	for root := range *d.stores {
		if idleSince(*d.used, root, now) {
			cands = append(cands, root)
		}
	}
	d.mu.Unlock()
	free, kept := splitHeld(cands, held)
	if len(kept) > 0 {
		d.mu.Lock()
		for _, root := range kept {
			if _, ok := (*d.stores)[root]; ok {
				*d.used = stampUsedLocked(*d.used, root) // the grace restarts on release
			}
		}
		d.mu.Unlock()
	}
	n := 0
	for _, root := range free {
		d.mu.Lock()
		s, ok := (*d.stores)[root]
		if !ok || !idleSince(*d.used, root, now) {
			d.mu.Unlock()
			continue // gone, or used since the candidates were taken
		}
		if _, leader := d.flight.begin(root); !leader {
			d.mu.Unlock()
			continue // an open or re-open is in flight; it is not idle
		}
		delete(*d.stores, root)
		delete(*d.used, root)
		if d.forget != nil {
			d.forget(root)
		}
		d.mu.Unlock()
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			defer func() {
				d.mu.Lock()
				d.flight.end(root)
				d.mu.Unlock()
			}()
			d.close(root, s)
		}()
		select {
		case <-closed:
		case <-time.After(storeCloseGrace):
			slog.Warn("stores: an idle store is slow to close; moving on", "root", root)
		}
		n++
	}
	return n
}

// stampUsedLocked records a use of root. Caller holds the owning pool's mutex.
func stampUsedLocked(used map[string]time.Time, root string) map[string]time.Time {
	if used == nil {
		used = make(map[string]time.Time)
	}
	used[root] = time.Now()
	return used
}

// idleSince reports whether root's last use is at least storeIdleGrace before
// now.
func idleSince(used map[string]time.Time, root string, now time.Time) bool {
	return now.Sub(used[root]) >= storeIdleGrace
}

// splitHeld parts the candidates into those no connection holds and those one
// does. held runs outside every pool mutex: it canonicalises paths, which
// touches the file system.
func splitHeld(cands []string, held func(root string) bool) (free, kept []string) {
	if held == nil {
		return cands, nil
	}
	for _, root := range cands {
		if held(root) {
			kept = append(kept, root)
		} else {
			free = append(free, root)
		}
	}
	return free, kept
}

// runStoreDrain is the daemon's janitor: every storeDrainEvery it drains the
// idle, unheld roots of each pool, until ctx ends.
func runStoreDrain(ctx context.Context, every time.Duration, topo *topologyPool, mem *memoryIndexPool, collabs *collabPool, held func() func(root string) bool) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			drainIdleStores(now, topo, mem, collabs, held())
		}
	}
}

// drainIdleStores is one janitor pass over the three pools.
func drainIdleStores(now time.Time, topo *topologyPool, mem *memoryIndexPool, collabs *collabPool, held func(root string) bool) {
	n := topo.reapIdle(now, held) + mem.reapIdle(now, held) + collabs.reapIdle(now, held)
	if n > 0 {
		slog.Info("stores: closed the databases of idle, unattached workspaces", "stores", n)
	}
}

// heldRootsOf builds the daemon's held predicate once per janitor pass: a root
// is held while a live connection's workspace or logical agent is on it
// (registry.heldRoots, snapshotted per pass) or its project-config watch is
// referenced.
func heldRootsOf(registry *connRegistry, watches *projectConfigWatchManager) func() func(root string) bool {
	return func() func(root string) bool {
		var live map[string]bool
		if registry != nil {
			live = registry.heldRoots()
		}
		return func(root string) bool {
			canon := paths.Canonical(root)
			return live[canon] || (watches != nil && watches.refs(canon) > 0)
		}
	}
}
