package cli

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
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

// TestHeldRootsOf: a root is held while a live connection's workspace or a
// logical agent is on it, or a project-config watch references it.
func TestHeldRootsOf(t *testing.T) {
	m := newProjectConfigWatchManager(t.Context(), func(string) {})
	defer m.close()
	reg := &connRegistry{conns: map[string]connHandle{}}
	conn, agent, watched, free := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	reg.add("s1", connHandle{heldRoots: func() []string { return []string{conn, agent} }})
	m.acquire(watched)
	defer m.release(watched)
	held := heldRootsOf(reg, m)()
	for root, want := range map[string]bool{conn: true, agent: true, watched: true, free: false} {
		if got := held(root); got != want {
			t.Errorf("held(%s) = %v, want %v", root, got, want)
		}
	}
	reg.remove("s1")
	if heldRootsOf(reg, m)()(conn) {
		t.Error("a root whose connection left is still held")
	}
}

// TestDrainIdle_ARootHeldRestartsItsGraceOnRelease: an attached session reads
// its cached handle without a pool lookup, so a held root's stamp is refreshed
// each pass — released, it gets the whole grace, not an immediate close.
func TestDrainIdle_ARootHeldRestartsItsGraceOnRelease(t *testing.T) {
	p := newTopologyPool(topoCfg())
	defer p.StopAll()
	root := t.TempDir()
	if p.Acquire(root, topoCfg()) == nil {
		t.Fatal("control: could not open")
	}
	p.mu.Lock()
	ageUse(p.used, root, 3*time.Hour) // attached for hours, no pool lookups
	p.mu.Unlock()
	if n := p.reapIdle(time.Now(), func(string) bool { return true }); n != 0 {
		t.Fatal("a held root was drained")
	}
	if n := p.reapIdle(time.Now().Add(time.Minute), nil); n != 0 {
		t.Fatal("a root released one pass ago was drained before its grace")
	}
	if n := p.reapIdle(time.Now().Add(storeIdleGrace+time.Minute), nil); n != 1 {
		t.Fatal("control: the released root was not drained after its grace")
	}
}

// TestDrainIdle_ASlowCloseDoesNotStallThePass: a wedged close is abandoned
// after storeCloseGrace (the flight stays claimed until it returns); the pass
// goes on to the next root.
func TestDrainIdle_ASlowCloseDoesNotStallThePass(t *testing.T) {
	var mu sync.Mutex
	var flight keyedFlight
	old := time.Now().Add(-storeIdleGrace)
	stores := map[string]int{"/slow": 1, "/fast": 2}
	used := map[string]time.Time{"/slow": old, "/fast": old}
	release := make(chan struct{})
	defer close(release)
	var fastClosed bool
	start := time.Now()
	n := drainIdle(drainSet[int]{
		mu: &mu, stores: &stores, used: &used, flight: &flight,
		close: func(root string, _ int) {
			if root == "/slow" {
				<-release
				return
			}
			fastClosed = true
		},
	}, time.Now(), nil)
	if n != 2 || !fastClosed {
		t.Fatalf("drained %d (fast closed %v), want 2 with the fast one closed", n, fastClosed)
	}
	if time.Since(start) > storeCloseGrace+2*time.Second {
		t.Fatal("the pass waited on the wedged close")
	}
	mu.Lock()
	_, leader := flight.begin("/slow")
	mu.Unlock()
	if leader {
		t.Fatal("the wedged root's flight was released before its close returned")
	}
}

// TestRunStoreDrain_DrainsEachPoolUntilCancelled pins the janitor: each tick
// drains all three pools with that tick's held predicate, and it stops with ctx.
func TestRunStoreDrain_DrainsEachPoolUntilCancelled(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	topo, mem, collabs := newTopologyPool(topoCfg()), newMemoryIndexPool(), newCollabPool()
	defer topo.StopAll()
	defer mem.CloseAll()
	defer collabs.closeAll()
	tRoot, mRoot, cRoot, kept := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	if topo.Acquire(tRoot, topoCfg()) == nil || mem.Acquire(mRoot) == nil || collabs.acquire(cRoot) == nil || topo.Acquire(kept, topoCfg()) == nil {
		t.Fatal("control: could not open the stores")
	}
	for _, age := range []func(){
		func() {
			topo.mu.Lock()
			ageUse(topo.used, tRoot, storeIdleGrace)
			ageUse(topo.used, kept, storeIdleGrace)
			topo.mu.Unlock()
		},
		func() { mem.mu.Lock(); ageUse(mem.used, mRoot, storeIdleGrace); mem.mu.Unlock() },
		func() { collabs.mu.Lock(); ageUse(collabs.used, cRoot, storeIdleGrace); collabs.mu.Unlock() },
	} {
		age()
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runStoreDrain(ctx, 10*time.Millisecond, topo, mem, collabs, func() func(string) bool {
			return func(root string) bool { return root == kept }
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (topo.isOpen(tRoot) || mem.indexCount() != 0 || collabs.storeCount() != 0) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if topo.isOpen(tRoot) || mem.indexCount() != 0 || collabs.storeCount() != 0 {
		t.Fatalf("the janitor did not drain every pool: topo %v mem %d collab %d", topo.isOpen(tRoot), mem.indexCount(), collabs.storeCount())
	}
	if !topo.isOpen(kept) {
		t.Fatal("the janitor ignored the held predicate")
	}
}

func (p *memoryIndexPool) indexCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.indexes)
}

func (p *collabPool) storeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.stores)
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

	topo.mu.Lock()
	ageUse(topo.used, root, storeIdleGrace)
	topo.mu.Unlock()
	if topo.forRoot(root) == nil {
		t.Fatal("control: forRoot did not find the store")
	}
	if n := topo.reapIdle(time.Now(), nil); n != 0 {
		t.Fatal("a topology root just reached through forRoot was drained")
	}

	mem := newMemoryIndexPool()
	defer mem.CloseAll()
	mws := t.TempDir()
	if mem.Acquire(mws) == nil {
		t.Fatal("control: could not open the memory index")
	}
	mem.mu.Lock()
	ageUse(mem.used, mws, storeIdleGrace)
	mem.mu.Unlock()
	mem.Acquire(mws)
	if n := mem.reapIdle(time.Now(), nil); n != 0 {
		t.Fatal("a memory index just acquired was drained")
	}
}

// isOpen reports whether root's store is published, without stamping a use.
func (p *topologyPool) isOpen(root string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.stores[root]
	return ok
}

// TestConnSession_HeldRootsIncludesItsAgents: a connection holds its own
// workspace and every logical agent's root.
func TestConnSession_HeldRootsIncludesItsAgents(t *testing.T) {
	s := &connSession{shards: map[string]*agentShard{"a1": {id: "a1", root: "/agent/root"}}}
	s.mutate(func(v *sessionView) { v.acquiredRoot = "/conn/root" })
	got := s.heldRoots()
	if !slices.Contains(got, "/conn/root") || !slices.Contains(got, "/agent/root") {
		t.Fatalf("heldRoots = %v, want the connection's and the agent's roots", got)
	}
}

// TestNewConnHandle_CarriesHeldRoots pins the registry wiring of the store
// drain's held set.
func TestNewConnHandle_CarriesHeldRoots(t *testing.T) {
	s := &connSession{}
	s.mutate(func(v *sessionView) { v.acquiredRoot = "/conn/root" })
	h := newConnHandle(s)
	if h.heldRoots == nil || !slices.Contains(h.heldRoots(), "/conn/root") {
		t.Fatal("the registry handle does not report the connection's held roots")
	}
}

// TestTopologyStoreForRoot_SkipsAClosedHandle: the root-keyed accessor never
// answers with the session's closed handle.
func TestTopologyStoreForRoot_SkipsAClosedHandle(t *testing.T) {
	base := config.Defaults()
	base.Topology = topoCfg()
	ws := t.TempDir()
	s := &connSession{store: config.NewStore(base), topologyPool: newTopologyPool(base.Topology)}
	defer s.topologyPool.StopAll()
	s.reconcileTopologyStore(ws)
	st := s.view().topologyStore
	if st == nil {
		t.Fatal("control: no topology store")
	}
	root := paths.Canonical(st.Root())
	if s.topologyStoreForRoot(root) == nil {
		t.Fatal("control: the accessor does not find the live store")
	}
	s.topologyPool.StopAll() // closes and unpublishes it; the view keeps the handle
	if got := s.topologyStoreForRoot(root); got != nil {
		t.Fatal("the root-keyed accessor answered with a closed store")
	}
}
