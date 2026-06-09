package cli

import (
	"context"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/config"
)

// hibernatePool builds a pool with one language whose idle_timeout / max are set
// by the test, so the janitor and LRU-eviction selection logic can be exercised
// without spawning real language servers.
func hibernatePool(language string, idle time.Duration, max int) *workspacePool {
	return &workspacePool{
		entries: make(map[poolKey]*poolEntry),
		baseCtx: context.Background(),
		langs: []langConfig{{name: language, cfg: config.LSPConfig{
			IdleTimeout:   config.Duration{Duration: idle},
			MaxWorkspaces: max,
		}}},
	}
}

// TestPool_TouchUpdatesLastUsed verifies the activity signal: touch advances an
// entry's lastUsed, and an unknown (root, language) is a safe no-op.
func TestPool_TouchUpdatesLastUsed(t *testing.T) {
	p := hibernatePool("java", time.Minute, 0)
	installEntryLang(p, "/root", "java", &stubClient{})

	before := p.lookup("/root", "java").lastUsed.Load()
	time.Sleep(2 * time.Millisecond)
	p.touch("/root", "java")
	after := p.lookup("/root", "java").lastUsed.Load()
	if after <= before {
		t.Fatalf("touch did not advance lastUsed: before=%d after=%d", before, after)
	}

	p.touch("/missing", "java") // must not panic
}

// TestPool_HibernateIdle_ReclaimsButKeepsEntry verifies the core hibernation
// invariant: an entry idle past its idle_timeout has its process stopped and its
// proxy cleared, but the poolEntry, its warm cache, and its map slot survive so
// the next acquire can restart it.
func TestPool_HibernateIdle_ReclaimsButKeepsEntry(t *testing.T) {
	p := hibernatePool("java", time.Millisecond, 0)
	cp := installEntryLang(p, "/root", "java", &stubClient{})
	e := p.lookup("/root", "java")
	warmCache := cache.New(time.Minute)
	e.cache = warmCache
	e.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())

	p.hibernateIdle()

	if got := p.lookup("/root", "java"); got != e {
		t.Fatal("entry was removed from the map; hibernation must keep it")
	}
	if e.state != poolHibernated {
		t.Fatalf("state = %v, want poolHibernated", e.state)
	}
	if cp.get() != nil {
		t.Fatal("proxy still live after hibernation; a routed call would hit a dying conn")
	}
	if e.cache != warmCache {
		t.Fatal("warm cache was replaced or dropped during hibernation")
	}
}

// TestPool_HibernateIdle_SkipsRecentlyActive verifies the janitor leaves a
// recently-used entry running.
func TestPool_HibernateIdle_SkipsRecentlyActive(t *testing.T) {
	p := hibernatePool("java", time.Hour, 0)
	cp := installEntryLang(p, "/root", "java", &stubClient{})
	e := p.lookup("/root", "java")
	e.lastUsed.Store(time.Now().UnixNano())

	p.hibernateIdle()

	if e.state != poolActive {
		t.Fatalf("state = %v, want poolActive (entry was recently used)", e.state)
	}
	if cp.get() == nil {
		t.Fatal("proxy cleared on a recently-active entry")
	}
}

// TestPool_HibernateIdle_SkipsZeroTimeout verifies that a language with
// idle_timeout = 0 never hibernates (the default for everything but java).
func TestPool_HibernateIdle_SkipsZeroTimeout(t *testing.T) {
	p := hibernatePool("go", 0, 0)
	installEntryLang(p, "/root", "go", &stubClient{})
	e := p.lookup("/root", "go")
	e.lastUsed.Store(time.Now().Add(-24 * time.Hour).UnixNano())

	p.hibernateIdle()

	if e.state != poolActive {
		t.Fatalf("state = %v, want poolActive (idle_timeout disabled)", e.state)
	}
}

// TestPool_OverBudgetVictim verifies LRU eviction selection: at/over the
// max_workspaces budget the least-recently-used running entry is the victim;
// under budget or with an unlimited cap, none is selected.
func TestPool_OverBudgetVictim(t *testing.T) {
	p := hibernatePool("java", time.Hour, 2)
	installEntryLang(p, "/a", "java", &stubClient{})
	installEntryLang(p, "/b", "java", &stubClient{})
	now := time.Now()
	p.lookup("/a", "java").lastUsed.Store(now.Add(-10 * time.Minute).UnixNano()) // oldest
	p.lookup("/b", "java").lastUsed.Store(now.UnixNano())

	if v := p.overBudgetVictimLocked("java", 0); v != nil {
		t.Fatal("unlimited cap (0) must select no victim")
	}
	if v := p.overBudgetVictimLocked("java", 3); v != nil {
		t.Fatal("under budget (2 running < 3) must select no victim")
	}
	v := p.overBudgetVictimLocked("java", 2)
	if v == nil || v.root != "/a" {
		t.Fatalf("victim = %v, want the LRU entry /a", v)
	}
}

// TestPool_HibernateAndWakeRestartsServer exercises the full hibernate→wake
// cycle against a real (no-op) supervised process: a pinned acquire starts a
// supervisor, hibernation stops it, and a later acquire restarts it on the same
// entry. Uses the `sleep` binary as a stand-in language server.
func TestPool_HibernateAndWakeRestartsServer(t *testing.T) {
	cmd, args := sleepCommand(t)
	pool := warmingPool(context.Background(), cmd, args)
	defer pool.close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // return fast: the no-op process never completes the handshake

	const root = "/tmp/plumb-hibernate-wake-root"
	e, err := pool.acquireLang(ctx, root, "go", true)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if e.sup == nil {
		t.Fatal("expected a live supervisor after acquire")
	}

	pool.hibernateEntry(e)
	if e.state != poolHibernated {
		t.Fatalf("state = %v, want poolHibernated", e.state)
	}

	woken, err := pool.acquireLang(ctx, root, "go", true)
	if err != nil {
		t.Fatalf("wake acquire: %v", err)
	}
	if woken != e {
		t.Fatal("wake created a new entry instead of restarting the existing one")
	}
	if e.state != poolActive {
		t.Fatalf("state = %v, want poolActive after wake", e.state)
	}
}
