package cli

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/fswatch"
	"github.com/plumbkit/plumb/internal/topology"
)

// TestWatchHub_APolyglotRootRunsOneOSWatcher is PLAN-503's measured drop: a
// Go+TypeScript workspace's topology index and its two language servers used
// to run three OS watchers over the same tree; through the hub they share one,
// and it closes with the last of them.
func TestWatchHub_APolyglotRootRunsOneOSWatcher(t *testing.T) {
	hub := fswatch.NewHub()
	ws := t.TempDir()
	cfg := config.Defaults().Topology
	cfg.Enabled, cfg.Watch = true, true
	store, err := topology.OpenWith(ws, cfg, nil, hub)
	if err != nil {
		t.Fatal(err)
	}
	if hub.Watches() == 0 {
		t.Skip("no file watcher on this platform")
	}
	goW, err := newLSPFSWatcherVia(hub, ws, &clientProxy{})
	if err != nil {
		t.Fatal(err)
	}
	tsW, err := newLSPFSWatcherVia(hub, ws, &clientProxy{})
	if err != nil {
		t.Fatal(err)
	}
	if n := hub.Watches(); n != 1 {
		t.Fatalf("OS watchers for one root with three consumers = %d, want 1", n)
	}
	goW.Start()
	tsW.Start()
	goW.Stop()
	tsW.Stop()
	if hub.Watches() != 1 {
		t.Fatal("the shared watcher closed while the topology index still used it")
	}
	_ = store.Close()
	if n := hub.Watches(); n != 0 {
		t.Fatalf("OS watchers after every consumer left = %d, want 0", n)
	}
}

// TestLSPWatchExclusion_SharedPlusExtraIsTheLanguageServersSet: the shared
// watcher's exclusion plus the LSP subscription's extra is exactly what the
// language servers excluded on their own watcher before.
func TestLSPWatchExclusion_SharedPlusExtraIsTheLanguageServersSet(t *testing.T) {
	ws := t.TempDir()
	shared := regexp.MustCompile(topology.SharedWatchOptions(ws).ExcludeRegex)
	extra := regexp.MustCompile(lspWatchExtraExcludeFor(ws))
	want := regexp.MustCompile(lspWatchExcludeRegexFor(ws))
	for _, rel := range []string{
		"main.go", "vendor/x.go", "node_modules/a.js", "testdata/f", "dist/b.js", "build/c", "target/d.o",
		"out/e", "__pycache__/f.pyc", ".git/HEAD", "src/target/g", "src/out.go", "pkg/outer/h.go",
	} {
		p := filepath.Join(ws, rel)
		got := shared.MatchString(p) || extra.MatchString(p)
		if got != want.MatchString(p) {
			t.Errorf("%s: shared+extra excludes %v, the language servers' set %v", rel, got, !got)
		}
	}
}
