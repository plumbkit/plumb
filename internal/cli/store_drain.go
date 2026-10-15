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
// "Held" is a connection attached to the root (its project-config watch
// reference, which every attach, re-pin and close keeps in step). A root reached
// only by an agent pinned elsewhere, by a cross-project note, or by a health
// probe is kept alive by use alone. The collab pool's daemon-level store is
// never drained.
//
// A caller that kept a store across a drain gets a clean error from the closed
// database, never a crash; the topology session handle re-acquires (see
// topologyStoreLive).

// storeIdleGrace is how long an unheld root's stores stay open after their last
// use; storeDrainEvery is how often the janitor looks.
const (
	storeIdleGrace  = 10 * time.Minute
	storeDrainEvery = time.Minute
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
	n := 0
	for _, root := range unheld(cands, held) {
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
		func() {
			defer func() {
				d.mu.Lock()
				d.flight.end(root)
				d.mu.Unlock()
			}()
			d.close(root, s)
		}()
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

// unheld drops the candidates a connection still holds. held runs outside every
// pool mutex: it canonicalises paths, which touches the file system.
func unheld(cands []string, held func(root string) bool) []string {
	if held == nil {
		return cands
	}
	out := cands[:0]
	for _, root := range cands {
		if !held(root) {
			out = append(out, root)
		}
	}
	return out
}

// runStoreDrain is the daemon's janitor: every storeDrainEvery it drains the
// idle, unheld roots of each pool, until ctx ends.
func runStoreDrain(ctx context.Context, topo *topologyPool, mem *memoryIndexPool, collabs *collabPool, held func(root string) bool) {
	t := time.NewTicker(storeDrainEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			drainIdleStores(now, topo, mem, collabs, held)
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

// heldByAConnection is the daemon's held predicate: a root is held while any
// connection's project-config watch references it.
func heldByAConnection(watches *projectConfigWatchManager) func(root string) bool {
	if watches == nil {
		return nil
	}
	return func(root string) bool {
		return watches.refs(paths.Canonical(root)) > 0
	}
}
