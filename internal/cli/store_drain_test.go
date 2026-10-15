package cli

import (
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
