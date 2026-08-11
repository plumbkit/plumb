package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/config"
)

// hibernatePool builds a pool with one language whose idle_timeout / maxWorkspaces are set
// by the test, so the janitor and LRU-eviction selection logic can be exercised
// without spawning real language servers.
func hibernatePool(language string, idle time.Duration, maxWorkspaces int) *workspacePool {
	return &workspacePool{
		entries: make(map[poolKey]*poolEntry),
		baseCtx: context.Background(),
		langs: []langConfig{{name: language, cfg: config.LSPConfig{
			IdleTimeout:   config.Duration{Duration: idle},
			MaxWorkspaces: maxWorkspaces,
		}}},
	}
}

// TestClientProxy_TouchUpdatesLastUsed verifies the activity signal: touch
// advances the proxy's lastUsed timestamp (the lock-free hot-path hook the
// janitor and LRU eviction read).
func TestClientProxy_TouchUpdatesLastUsed(t *testing.T) {
	cp := &clientProxy{}
	before := cp.lastUsed.Load()
	time.Sleep(2 * time.Millisecond)
	cp.touch()
	if cp.lastUsed.Load() <= before {
		t.Fatalf("touch did not advance lastUsed: before=%d after=%d", before, cp.lastUsed.Load())
	}
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
	e.proxy.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())

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
	e.proxy.lastUsed.Store(time.Now().UnixNano())

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
	e.proxy.lastUsed.Store(time.Now().Add(-24 * time.Hour).UnixNano())

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
	p.lookup("/a", "java").proxy.lastUsed.Store(now.Add(-10 * time.Minute).UnixNano()) // oldest
	p.lookup("/b", "java").proxy.lastUsed.Store(now.UnixNano())

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

// TestPool_PruneServerStateCaches verifies cache maintenance: an unused state
// dir older than the age threshold is removed, a recent one is kept, and a dir
// backing a pooled Java workspace is kept even when it is old.
func TestPool_PruneServerStateCaches(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	base := filepath.Join(config.CacheDir(), "jdtls-data")
	mustMkdir(t, base)
	old := time.Now().Add(-40 * 24 * time.Hour)

	stale := filepath.Join(base, "stalehash")
	mustMkdir(t, stale)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	recent := filepath.Join(base, "recenthash")
	mustMkdir(t, recent)

	p := hibernatePool("java", time.Hour, 0)
	const root = "/some/java/project"
	installEntryLang(p, root, "java", &stubClient{})
	inUse := serverStateDir("java", root)
	mustMkdir(t, inUse)
	if err := os.Chtimes(inUse, old, old); err != nil { // old, but live ⇒ must be kept
		t.Fatal(err)
	}

	p.pruneServerStateCaches()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale unused jdtls-data dir was not pruned")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent jdtls-data dir was wrongly pruned")
	}
	if _, err := os.Stat(inUse); err != nil {
		t.Error("in-use jdtls-data dir was wrongly pruned")
	}
}

// TestPool_PruneServerStateCaches_CoversEveryLanguage pins that pruning follows
// serverStateDirs rather than naming one language: a server added to that table
// without pruning would accumulate a directory per project ever opened, behind a
// distribution measured in gigabytes.
func TestPool_PruneServerStateCaches_CoversEveryLanguage(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	old := time.Now().Add(-40 * 24 * time.Hour)

	stale := map[string]string{}
	for language, spec := range serverStateDirs {
		dir := filepath.Join(config.CacheDir(), spec.subdir, "stalehash-"+language)
		mustMkdir(t, dir)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
		stale[language] = dir
	}
	if len(stale) < 2 {
		t.Fatalf("serverStateDirs covers %d languages; this test is only meaningful for several", len(stale))
	}

	hibernatePool("java", time.Hour, 0).pruneServerStateCaches()

	for language, dir := range stale {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s: stale state dir %s was not pruned", language, dir)
		}
	}
}

// TestArgsFor_AppendsPerRootStateDir pins that each language in serverStateDirs
// gets its own flag and its own per-root directory, and that a language outside
// the table is passed through untouched. The kotlin row is the reason this is a
// table at all: --system-path is derived from the workspace root, so it cannot
// live in [lsp.kotlin] args.
func TestArgsFor_AppendsPerRootStateDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	for language, spec := range serverStateDirs {
		t.Run(language, func(t *testing.T) {
			base := []string{"--stdio"}
			got := argsFor(language, "/projects/one", config.LSPConfig{Args: base})
			if len(got) != len(base)+2 || got[len(got)-2] != spec.flag {
				t.Fatalf("argsFor(%s) = %v, want the configured args plus %q <dir>", language, got, spec.flag)
			}
			if got[0] != "--stdio" {
				t.Errorf("configured args were not preserved: %v", got)
			}
			other := argsFor(language, "/projects/two", config.LSPConfig{Args: base})
			if got[len(got)-1] == other[len(other)-1] {
				t.Errorf("two roots share the state dir %q — they would fight over one index", got[len(got)-1])
			}
			if _, err := os.Stat(got[len(got)-1]); err != nil {
				t.Errorf("state dir was not created: %v", err)
			}
		})
	}

	if got := argsFor("go", "/projects/one", config.LSPConfig{Args: []string{"-rpc.trace"}}); len(got) != 1 {
		t.Errorf("argsFor(go) = %v, want the configured args verbatim", got)
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
