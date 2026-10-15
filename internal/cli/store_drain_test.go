package cli

import (
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// These tests pin PLAN-535: an idle, unheld root's topology, memory and collab
// stores are closed after the grace; a held or recently used one stays open;
// the next use re-opens; the daemon-level collab store is never drained.

func ageUse(used map[string]time.Time, root string, by time.Duration) {
	used[root] = used[root].Add(-by)
}

func topoCfg() config.TopologyConfig {
	cfg := config.Defaults().Topology
	cfg.Enabled = true
	cfg.Watch = false // no OS watcher in a unit test
	return cfg
}

func TestTopologyPool_DrainsAnIdleUnheldRoot(t *testing.T) {
	p := newTopologyPool(topoCfg())
	defer p.StopAll()
	idle, busy, held := t.TempDir(), t.TempDir(), t.TempDir()
	stores := map[string]any{}
	for _, root := range []string{idle, busy, held} {
		s := p.Acquire(root, topoCfg())
		if s == nil {
			t.Fatalf("control: could not open %s", root)
		}
		stores[root] = s
	}
	p.mu.Lock()
	ageUse(p.used, idle, storeIdleGrace)
	ageUse(p.used, held, storeIdleGrace)
	p.mu.Unlock()
	now := time.Now()
	isHeld := func(root string) bool { return root == held }
	if n := p.reapIdle(now, isHeld); n != 1 {
		t.Fatalf("drained %d stores, want 1 (the idle, unheld root)", n)
	}
	if p.get(idle) != nil {
		t.Fatal("the idle root's store is still published")
	}
	if p.get(busy) == nil || p.get(held) == nil {
		t.Fatal("a recently used or held root was drained")
	}
	reopened := p.Acquire(idle, topoCfg())
	if reopened == nil || reopened.Closed() || any(reopened) == stores[idle] {
		t.Fatal("the next Acquire did not re-open the drained root")
	}
}

func TestTopologyStoreLive_ReacquiresAClosedStore(t *testing.T) {
	base := config.Defaults()
	base.Topology = topoCfg()
	ws := t.TempDir()
	s := &connSession{store: config.NewStore(base), topologyPool: newTopologyPool(base.Topology)}
	defer s.topologyPool.StopAll()
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })
	s.reconcileTopologyStore(ws)
	first := s.view().topologyStore
	if first == nil {
		t.Fatal("control: no topology store")
	}
	s.topologyPool.mu.Lock()
	ageUse(s.topologyPool.used, ws, storeIdleGrace)
	s.topologyPool.mu.Unlock()
	s.topologyPool.reapIdle(time.Now(), nil)
	if !first.Closed() {
		t.Fatal("control: the drain did not close the store")
	}
	if got := s.topologyStoreLive(); got == nil || got.Closed() || got == first {
		t.Fatal("the session handed out its closed topology store instead of re-acquiring")
	}
}

func TestMemoryPool_DrainsAnIdleUnheldWorkspace(t *testing.T) {
	p := newMemoryIndexPool()
	defer p.CloseAll()
	idle, held := t.TempDir(), t.TempDir()
	first := p.Acquire(idle)
	if first == nil || p.Acquire(held) == nil {
		t.Fatal("control: could not open the indexes")
	}
	p.mu.Lock()
	ageUse(p.used, idle, storeIdleGrace)
	ageUse(p.used, held, storeIdleGrace)
	p.mu.Unlock()
	if n := p.reapIdle(time.Now(), func(root string) bool { return root == held }); n != 1 {
		t.Fatalf("drained %d indexes, want 1", n)
	}
	p.mu.Lock()
	_, stillIdle := p.indexes[idle]
	_, stillHeld := p.indexes[held]
	p.mu.Unlock()
	if stillIdle || !stillHeld {
		t.Fatalf("published after the drain: idle %v (want false), held %v (want true)", stillIdle, stillHeld)
	}
	if again := p.Acquire(idle); again == nil || again == first {
		t.Fatal("the next Acquire did not re-open the drained workspace")
	}
}

func TestCollabPool_DrainsAnIdleWorkspaceButNeverTheGlobalStore(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	p := newCollabPool()
	defer p.closeAll()
	ws := t.TempDir()
	if p.acquire(ws) == nil || p.acquireGlobal() == nil {
		t.Fatal("control: could not open the stores")
	}
	p.mu.Lock()
	ageUse(p.used, ws, storeIdleGrace)
	p.mu.Unlock()
	if n := p.reapIdle(time.Now(), nil); n != 1 {
		t.Fatalf("drained %d stores, want 1 (the workspace's)", n)
	}
	p.mu.Lock()
	_, open := p.stores[ws]
	global := p.global
	p.mu.Unlock()
	if open {
		t.Fatal("the idle workspace's store is still published")
	}
	if global == nil {
		t.Fatal("the daemon-level store was drained")
	}
	_ = p.acquire(ws) // re-opens
}

// TestDrainIdleStores_SparesARecentUse: a lookup after the candidates were
// taken keeps the root (the re-check under the lock).
func TestDrainIdleStores_SparesARecentUse(t *testing.T) {
	p := newTopologyPool(topoCfg())
	defer p.StopAll()
	root := t.TempDir()
	if p.Acquire(root, topoCfg()) == nil {
		t.Fatal("control: could not open")
	}
	p.mu.Lock()
	ageUse(p.used, root, storeIdleGrace)
	p.mu.Unlock()
	now := time.Now()
	used := func(string) bool { // a lookup lands between the candidate scan and the drain
		p.get(root)
		return false
	}
	if n := p.reapIdle(now.Add(time.Second), used); n != 0 {
		t.Fatal("a root used during the drain was closed")
	}
}

// TestHeldByAConnection: a root is held exactly while a connection's
// project-config watch references it.
func TestHeldByAConnection(t *testing.T) {
	if heldByAConnection(nil) != nil {
		t.Fatal("no watch manager must mean no held predicate")
	}
	m := newProjectConfigWatchManager(t.Context(), func(string) {})
	defer m.close()
	root := t.TempDir()
	held := heldByAConnection(m)
	if held(root) {
		t.Fatal("control: an unwatched root is held")
	}
	m.acquire(root)
	if !held(root) {
		t.Fatal("a root a connection watches is not held")
	}
	m.release(root)
	if held(root) {
		t.Fatal("a released root is still held")
	}
}

// TestDrainIdle_HoldsTheFlightWhileClosing: an open of a root being drained
// waits for the close (it would otherwise open a second handle on the database
// the drain is still closing).
func TestDrainIdle_HoldsTheFlightWhileClosing(t *testing.T) {
	var mu sync.Mutex
	var flight keyedFlight
	stores := map[string]int{"/r": 1}
	used := map[string]time.Time{"/r": time.Now().Add(-storeIdleGrace)}
	closing, release := make(chan struct{}), make(chan struct{})
	done := make(chan int, 1)
	go func() {
		done <- drainIdle(drainSet[int]{
			mu: &mu, stores: &stores, used: &used, flight: &flight,
			close: func(string, int) { close(closing); <-release },
		}, time.Now(), nil)
	}()
	<-closing
	mu.Lock()
	wait, leader := flight.begin("/r")
	mu.Unlock()
	if leader {
		t.Fatal("an open of a root being drained did not wait for the close")
	}
	close(release)
	<-wait
	if n := <-done; n != 1 {
		t.Fatalf("drained %d, want 1", n)
	}
}

// TestPools_ALookupKeepsARootAlive: a lookup (topology Acquire, collab
// open) refreshes the root's use, so the drain spares it.
func TestPools_ALookupKeepsARootAlive(t *testing.T) {
	topo := newTopologyPool(topoCfg())
	defer topo.StopAll()
	root := t.TempDir()
	if topo.Acquire(root, topoCfg()) == nil {
		t.Fatal("control: could not open")
	}
	topo.mu.Lock()
	ageUse(topo.used, root, storeIdleGrace)
	topo.mu.Unlock()
	topo.Acquire(root, topoCfg())
	if n := topo.reapIdle(time.Now(), nil); n != 0 {
		t.Fatal("a topology root just acquired was drained")
	}

	t.Setenv("XDG_DATA_HOME", t.TempDir())
	collabs := newCollabPool()
	defer collabs.closeAll()
	ws := t.TempDir()
	if collabs.acquire(ws) == nil {
		t.Fatal("control: could not open the collab store")
	}
	collabs.mu.Lock()
	ageUse(collabs.used, ws, storeIdleGrace)
	collabs.mu.Unlock()
	collabs.get(ws)
	if n := collabs.reapIdle(time.Now(), nil); n != 0 {
		t.Fatal("a collab store just looked up was drained")
	}
}
