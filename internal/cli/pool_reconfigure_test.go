package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp"
)

// These tests pin PLAN-539: an [lsp.<lang>] change restarts exactly the servers
// whose effective config it changes, in place and only once no call holds them.

// reconfigPool is a real pool whose "go" and "python" servers are sleep
// processes that never finish the handshake.
func reconfigPool(t *testing.T) (*workspacePool, config.Config) {
	t.Helper()
	sleep, _ := sleepCommand(t)
	cfg := config.Defaults()
	cfg.LSP = map[string]config.LSPConfig{
		"go":     {Command: sleep, Args: []string{"30"}, Enabled: true, Diagnostics: "push", RootMarkers: []string{"go.mod"}},
		"python": {Command: sleep, Args: []string{"31"}, Enabled: true, Diagnostics: "push", RootMarkers: []string{"pyproject.toml"}},
	}
	p := newWorkspacePool(context.Background(), cfg)
	p.startGrace = 20 * time.Millisecond
	t.Cleanup(p.close)
	return p, cfg
}

func markedRoot(t *testing.T, marker string) string {
	t.Helper()
	dir := freshTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, marker), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// entryNow reads an entry's supervisor and config under the pool lock.
func entryNow(p *workspacePool, e *poolEntry) (*lsp.Supervisor, config.LSPConfig, poolLifecycle) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return e.sup, e.lspCfg, e.state
}

func waitReconfigured(t *testing.T, p *workspacePool, e *poolEntry, ok func(config.LSPConfig) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, cfg, _ := entryNow(p, e); ok(cfg) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("entry %s/%s was not restarted with its new config", e.root, e.language)
}

func TestApplyGlobalLSP_RestartsOnlyTheChangedLanguage(t *testing.T) {
	p, cfg := reconfigPool(t)
	var watchers atomic.Int32
	p.watcherFactory = func(string, *clientProxy) (*lspFSWatcher, error) {
		watchers.Add(1)
		return nil, errSeam
	}
	ctx := context.Background()
	goEntry, err := p.acquireLang(ctx, markedRoot(t, "go.mod"), "go", true)
	if err != nil {
		t.Fatal(err)
	}
	pyEntry, err := p.acquireLang(ctx, markedRoot(t, "pyproject.toml"), "python", true)
	if err != nil {
		t.Fatal(err)
	}
	goSup, _, _ := entryNow(p, goEntry)
	pySup, _, _ := entryNow(p, pyEntry)
	builtBefore := watchers.Load()
	genBefore := p.langsGen.Load()
	p.mu.Lock()
	goEntry.diagDowngraded = true
	goEntry.goWorkOff = "/stale/go.work"
	p.mu.Unlock()
	goEntry.cache.Set("stale", 1, time.Minute)

	next := cfg
	next.LSP = map[string]config.LSPConfig{"go": cfg.LSP["go"], "python": cfg.LSP["python"]}
	g := next.LSP["go"]
	g.Args = []string{"32"}
	next.LSP["go"] = g
	if n := p.applyGlobalLSP(next); n != 1 {
		t.Fatalf("restarts started = %d, want 1 (the Go server only)", n)
	}
	waitReconfigured(t, p, goEntry, func(c config.LSPConfig) bool { return slices.Equal(c.Args, []string{"32"}) })
	if sup, _, state := entryNow(p, goEntry); sup == goSup || !state.running() {
		t.Fatalf("the Go server was not restarted in place and woken (same supervisor %v, state %s)", sup == goSup, state)
	}
	p.mu.Lock()
	downgraded, goWork := goEntry.diagDowngraded, goEntry.goWorkOff
	p.mu.Unlock()
	if downgraded || goWork == "/stale/go.work" {
		t.Fatalf("the new server inherited the old one's state (diag downgrade %v, go.work %q)", downgraded, goWork)
	}
	if _, ok := goEntry.cache.Get("stale"); ok {
		t.Fatal("the new server kept the old one's cached answers")
	}
	if p.langsGen.Load() == genBefore {
		t.Fatal("a global [lsp] change did not bump the language generation")
	}
	deadline := time.Now().Add(5 * time.Second)
	for watchers.Load() == builtBefore && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if watchers.Load() == builtBefore {
		t.Fatal("the woken server's file watcher was not rebuilt")
	}
	if sup, c, _ := entryNow(p, pyEntry); sup != pySup || !slices.Equal(c.Args, []string{"31"}) {
		t.Fatal("the Python server was restarted by a Go-only change")
	}
	if n := p.applyGlobalLSP(next); n != 0 {
		t.Fatalf("re-applying the same config started %d restarts, want 0", n)
	}
}

// TestApplyGlobalLSP_WaitsForTheCallInProgress: a call holding the server is
// never cut short; the restart follows its end.
func TestApplyGlobalLSP_WaitsForTheCallInProgress(t *testing.T) {
	p, cfg := reconfigPool(t)
	ctx := context.Background()
	root := markedRoot(t, "go.mod")
	e, done, err := p.acquireCall(ctx, root, "go")
	if err != nil {
		t.Fatal(err)
	}
	sup, _, _ := entryNow(p, e)
	next := cfg
	next.LSP = map[string]config.LSPConfig{"python": cfg.LSP["python"], "go": cfg.LSP["go"]}
	g := next.LSP["go"]
	g.Diagnostics = "pull"
	next.LSP["go"] = g
	p.applyGlobalLSP(next)
	time.Sleep(150 * time.Millisecond)
	if now, _, _ := entryNow(p, e); now != sup {
		t.Fatal("the server was restarted while a call held it")
	}
	done()
	waitReconfigured(t, p, e, func(c config.LSPConfig) bool { return c.Diagnostics == "pull" })
}

// TestApplyGlobalLSP_KeepsALiveEnable: a language turned on with enable-lsp
// stays on across a global reload that the config file does not mention.
func TestApplyGlobalLSP_KeepsALiveEnable(t *testing.T) {
	p, cfg := reconfigPool(t)
	py := cfg.LSP["python"]
	py.Enabled = false
	off := cfg
	off.LSP = map[string]config.LSPConfig{"go": cfg.LSP["go"], "python": py} // a fresh map: the pool shares cfg's
	p.applyGlobalLSP(off)
	if _, err := p.enableLanguage("python"); err != nil {
		t.Fatal(err)
	}
	reload := off
	g := reload.LSP["go"]
	g.Diagnostics = "pull" // an unrelated change, so the reload is real
	reload.LSP = map[string]config.LSPConfig{"go": g, "python": py}
	p.applyGlobalLSP(reload)
	found := false
	for _, l := range p.langsSnapshot() {
		if l.name == "python" {
			found = true
		}
	}
	if !found {
		t.Fatal("a global reload undid `plumb enable-lsp python`")
	}
}

// TestApplyGlobalLSP_DisabledLanguageStops: a language the new config disables
// has its servers stopped, and they stay stopped.
func TestApplyGlobalLSP_DisabledLanguageStops(t *testing.T) {
	p, cfg := reconfigPool(t)
	e, err := p.acquireLang(context.Background(), markedRoot(t, "go.mod"), "go", true)
	if err != nil {
		t.Fatal(err)
	}
	next := cfg
	g := cfg.LSP["go"]
	g.Enabled = false
	next.LSP = map[string]config.LSPConfig{"go": g, "python": cfg.LSP["python"]}
	p.applyGlobalLSP(next)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, state := entryNow(p, e); state == poolHibernated {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a disabled language's server was not stopped")
}

// TestInvalidatePoolLanguages_ReconfiguresTheProject: a project-config reload
// restarts that root's servers whose effective config it changed.
func TestInvalidatePoolLanguages_ReconfiguresTheProject(t *testing.T) {
	p, _ := reconfigPool(t)
	root := markedRoot(t, "go.mod")
	e, err := p.acquireLang(context.Background(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	writePoolProjectConfig(t, root, "[lsp.go]\ndiagnostics = \"pull\"\n")
	s := &connSession{pool: p}
	s.invalidatePoolLanguages(root)
	waitReconfigured(t, p, e, func(c config.LSPConfig) bool { return c.Diagnostics == "pull" })
}

// TestApplyGlobalLSP_PoolKnobsApplyWithoutARestart: an idle-timeout or
// server-cap change is adopted in place; the server keeps running.
func TestApplyGlobalLSP_PoolKnobsApplyWithoutARestart(t *testing.T) {
	p, cfg := reconfigPool(t)
	e, err := p.acquireLang(context.Background(), markedRoot(t, "go.mod"), "go", true)
	if err != nil {
		t.Fatal(err)
	}
	sup, _, _ := entryNow(p, e)
	next := cfg
	g := cfg.LSP["go"]
	g.IdleTimeout = config.Duration{Duration: 20 * time.Minute}
	g.MaxWorkspaces = 2
	next.LSP = map[string]config.LSPConfig{"go": g, "python": cfg.LSP["python"]}
	if n := p.applyGlobalLSP(next); n != 0 {
		t.Fatalf("restarts started = %d, want 0 for pool knobs alone", n)
	}
	now, c, _ := entryNow(p, e)
	if now != sup {
		t.Fatal("the server was restarted for an idle-timeout/cap change")
	}
	if c.IdleTimeout.Duration != 20*time.Minute || c.MaxWorkspaces != 2 {
		t.Fatalf("the entry did not adopt the new knobs: %+v", c)
	}
}

// TestGlobalConfigReconciler_ReachesThePool pins the daemon's wiring: a global
// config change reaches the language-server pool.
func TestGlobalConfigReconciler_ReachesThePool(t *testing.T) {
	p, cfg := reconfigPool(t)
	topo := newTopologyPool(cfg.Topology)
	t.Cleanup(topo.StopAll)
	next := cfg
	g := cfg.LSP["go"]
	g.Args = []string{"33"}
	next.LSP = map[string]config.LSPConfig{"go": g, "python": cfg.LSP["python"]}
	globalConfigReconciler(config.NewStore(cfg), topo, p)(next)
	if got := p.snapshotBaseConfig().LSP["go"].Args; !slices.Equal(got, []string{"33"}) {
		t.Fatalf("the pool did not adopt the new [lsp.go] config: args %v", got)
	}
}

// TestApplyGlobalLSP_GivesUpOnATornDownEntry: a restart waiting for a call to
// end stops waiting at once when the entry is torn down meanwhile, instead of
// polling out its whole wait.
func TestApplyGlobalLSP_GivesUpOnATornDownEntry(t *testing.T) {
	p, cfg := reconfigPool(t)
	root := markedRoot(t, "go.mod")
	e, done, err := p.acquireCall(context.Background(), root, "go")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	next := cfg
	g := cfg.LSP["go"]
	g.Args = []string{"34"}
	next.LSP = map[string]config.LSPConfig{"go": g, "python": cfg.LSP["python"]}
	if n := p.applyGlobalLSP(next); n != 1 {
		t.Fatalf("restarts started = %d, want 1", n)
	}
	p.mu.Lock()
	delete(p.entries, poolKey{root, "go"})
	p.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		busy := e.reconfiguring
		p.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the restart kept waiting on an entry that was torn down")
}

// TestRestartWithConfig_DoesNotReviveAfterClose: a restart that reaches its
// swap after the pool closed installs no new server and wakes nothing.
func TestRestartWithConfig_DoesNotReviveAfterClose(t *testing.T) {
	p, cfg := reconfigPool(t)
	e, err := p.acquireLang(context.Background(), markedRoot(t, "go.mod"), "go", true)
	if err != nil {
		t.Fatal(err)
	}
	sup, _, _ := entryNow(p, e)
	p.close()
	g := cfg.LSP["go"]
	g.Args = []string{"35"}
	p.restartWithConfig(e, g, true)
	if now, c, state := entryNow(p, e); now != sup || slices.Equal(c.Args, []string{"35"}) || state.running() {
		t.Fatalf("a closed pool's entry was restarted (new supervisor %v, args %v, state %s)", now != sup, c.Args, state)
	}
}

// withGoArgs returns cfg with the Go server's args set (a fresh map).
func withGoArgs(cfg config.Config, args ...string) config.Config {
	g := cfg.LSP["go"]
	g.Args = args
	cfg.LSP = map[string]config.LSPConfig{"go": g, "python": cfg.LSP["python"]}
	return cfg
}

// TestApplyGlobalLSP_ConvergesOnTheLatestChange: a second change that arrives
// while the first restart waits on a call is not lost — the entry ends on the
// latest config.
func TestApplyGlobalLSP_ConvergesOnTheLatestChange(t *testing.T) {
	p, cfg := reconfigPool(t)
	e, done, err := p.acquireCall(context.Background(), markedRoot(t, "go.mod"), "go")
	if err != nil {
		t.Fatal(err)
	}
	p.applyGlobalLSP(withGoArgs(cfg, "40"))
	p.applyGlobalLSP(withGoArgs(cfg, "41"))
	done()
	waitReconfigured(t, p, e, func(c config.LSPConfig) bool { return slices.Equal(c.Args, []string{"41"}) })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		busy := e.reconfiguring
		p.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the restart never finished after converging")
}

// TestReconfigureRoot_IgnoresAnUnparsableProjectConfig: a half-typed project
// file does not flip the servers onto the global fallback.
func TestReconfigureRoot_IgnoresAnUnparsableProjectConfig(t *testing.T) {
	p, _ := reconfigPool(t)
	p.baseConfig.LogLevel = "info" // the project-config resolver is active
	root := markedRoot(t, "go.mod")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writePoolProjectConfig(t, root, "[lsp.go]\ndiagnostics = \"pull\"\n")
	e, err := p.acquireLang(context.Background(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, c, _ := entryNow(p, e); c.Diagnostics != "pull" {
		t.Fatalf("control: the project's diagnostics = %q, want pull", c.Diagnostics)
	}
	sup, _, _ := entryNow(p, e)
	writePoolProjectConfig(t, root, "[lsp.go\ndiagnostics = \n")
	p.invalidateLanguageConfig(root)
	if n := p.reconfigureRoot(root); n != 0 {
		t.Fatalf("restarts started = %d for an unparsable project config, want 0", n)
	}
	if now, c, _ := entryNow(p, e); now != sup || c.Diagnostics != "pull" {
		t.Fatalf("the server moved onto the global fallback (new supervisor %v, diagnostics %q)", now != sup, c.Diagnostics)
	}
}

// TestApplyGlobalLSP_FileEditOfEnabledOverridesALiveEnable: after enable-lsp,
// a reload that leaves `enabled` alone keeps the language on, but one whose
// file edit turns it off wins.
func TestApplyGlobalLSP_FileEditOfEnabledOverridesALiveEnable(t *testing.T) {
	p, cfg := reconfigPool(t)
	py := cfg.LSP["python"]
	py.Enabled = false
	off := cfg
	off.LSP = map[string]config.LSPConfig{"go": cfg.LSP["go"], "python": py}
	p.applyGlobalLSP(off)
	if _, err := p.enableLanguage("python"); err != nil {
		t.Fatal(err)
	}
	has := func() bool {
		for _, l := range p.langsSnapshot() {
			if l.name == "python" {
				return true
			}
		}
		return false
	}
	py.Enabled = true // the file now says on...
	on := off
	on.LSP = map[string]config.LSPConfig{"go": cfg.LSP["go"], "python": py}
	p.applyGlobalLSP(on)
	py.Enabled = false // ...and then off again: an edit, newer than the enable
	again := off
	again.LSP = map[string]config.LSPConfig{"go": cfg.LSP["go"], "python": py}
	p.applyGlobalLSP(again)
	if has() {
		t.Fatal("a file edit turning python off was overridden by an older enable-lsp")
	}
}

// TestApplyGlobalLSP_RootMarkersRestartNothing: root markers only decide where
// a server starts, so changing them restarts no running server.
func TestApplyGlobalLSP_RootMarkersRestartNothing(t *testing.T) {
	p, cfg := reconfigPool(t)
	e, err := p.acquireLang(context.Background(), markedRoot(t, "go.mod"), "go", true)
	if err != nil {
		t.Fatal(err)
	}
	sup, _, _ := entryNow(p, e)
	next := cfg
	g := cfg.LSP["go"]
	g.RootMarkers = []string{"go.mod", "go.work"}
	next.LSP = map[string]config.LSPConfig{"go": g, "python": cfg.LSP["python"]}
	if n := p.applyGlobalLSP(next); n != 0 {
		t.Fatalf("restarts started = %d for a root-marker change, want 0", n)
	}
	if now, _, _ := entryNow(p, e); now != sup {
		t.Fatal("a root-marker change restarted the server")
	}
}

// TestApplyGlobalLSP_RacesNothing runs reloads against live enables; -race
// is the assertion (applyGlobalLSP publishes under p.mu and langsMu).
func TestApplyGlobalLSP_RacesNothing(t *testing.T) {
	p, cfg := reconfigPool(t)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 50 {
			p.applyGlobalLSP(withGoArgs(cfg, strconv.Itoa(i)))
		}
	})
	wg.Go(func() {
		for range 50 {
			_, _ = p.enableLanguage("python")
		}
	})
	wg.Wait()
}

// TestApplyGlobalLSP_ReachesProjectGovernedRoots: a global change reaches a
// root whose languages are resolved through its project config — the cached
// project resolution is dropped, not served stale.
func TestApplyGlobalLSP_ReachesProjectGovernedRoots(t *testing.T) {
	p, cfg := reconfigPool(t)
	p.baseConfig.LogLevel = "info" // the project-config resolver is active
	root := markedRoot(t, "go.mod")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writePoolProjectConfig(t, root, "[edits]\nrate_limit_per_minute = 100\n")
	e, err := p.acquireLang(context.Background(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	if n := p.applyGlobalLSP(withGoArgs(cfg, "42")); n != 1 {
		t.Fatalf("restarts started = %d, want 1 for the project-governed root", n)
	}
	waitReconfigured(t, p, e, func(c config.LSPConfig) bool { return slices.Equal(c.Args, []string{"42"}) })
}
