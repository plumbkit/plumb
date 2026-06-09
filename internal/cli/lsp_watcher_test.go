package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sgtdi/fswatcher"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func TestLSPWatchShouldSkipPath(t *testing.T) {
	cases := []struct {
		rel  string
		skip bool
	}{
		{"", true},
		{".", true},
		{"..", true},
		{"../escape", true},
		{".git/HEAD", true},
		{".plumb/config.toml", true},
		{".hidden/file", true},
		{"vendor/foo.go", true},
		{"node_modules/x.js", true},
		{"testdata/golden.txt", true},
		{"dist/bundle.js", true},
		{"build/out", true},
		{"target/debug/foo", true},
		{"out/Release/x", true},
		{"__pycache__/x.cpython.pyc", true},
		{"src/.cache/x", true},
		{"src/main.go", false},
		{"pkg/foo/bar.go", false},
		{"a/b/c.py", false},
	}
	for _, tc := range cases {
		if got := lspWatchShouldSkipPath(tc.rel); got != tc.skip {
			t.Errorf("lspWatchShouldSkipPath(%q) = %v, want %v", tc.rel, got, tc.skip)
		}
	}
}

func TestLSPFileChangeType(t *testing.T) {
	cases := []struct {
		name  string
		types []fswatcher.EventType
		want  protocol.FileChangeType
	}{
		{"remove wins", []fswatcher.EventType{fswatcher.EventCreate, fswatcher.EventRemove}, protocol.FileDeleted},
		{"create only", []fswatcher.EventType{fswatcher.EventCreate}, protocol.FileCreated},
		{"mod only", []fswatcher.EventType{fswatcher.EventMod}, protocol.FileChanged},
		{"chmod only", []fswatcher.EventType{fswatcher.EventChmod}, protocol.FileChanged},
		{"unknown only", []fswatcher.EventType{fswatcher.EventUnknown}, protocol.FileChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lspFileChangeType(fswatcher.WatchEvent{Types: tc.types})
			if got != tc.want {
				t.Errorf("lspFileChangeType(%v) = %v, want %v", tc.types, got, tc.want)
			}
		})
	}
}

func TestLSPWatchHasOverflow(t *testing.T) {
	if !lspWatchHasOverflow(fswatcher.WatchEvent{Types: []fswatcher.EventType{fswatcher.EventOverflow}}) {
		t.Error("expected overflow detected")
	}
	if !lspWatchHasOverflow(fswatcher.WatchEvent{Types: []fswatcher.EventType{fswatcher.EventMod, fswatcher.EventOverflow}}) {
		t.Error("expected overflow detected among mixed types")
	}
	if lspWatchHasOverflow(fswatcher.WatchEvent{Types: []fswatcher.EventType{fswatcher.EventMod}}) {
		t.Error("expected no overflow on a plain mod event")
	}
}

// TestLSPFSWatcher_StartStopLifecycle confirms Start/Stop is idempotent and
// the consume goroutines exit promptly when Stop is called.
func TestLSPFSWatcher_StartStopLifecycle(t *testing.T) {
	dir := t.TempDir()
	proxy := &clientProxy{}
	fw, err := newLSPFSWatcher(dir, proxy)
	if err != nil {
		t.Fatalf("newLSPFSWatcher: %v", err)
	}
	fw.Start()

	// Second Stop must be a no-op.
	done := make(chan struct{})
	go func() {
		fw.Stop()
		fw.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s — consume loop leaked")
	}
}

// TestLSPFSWatcher_NoClient_NoCrash verifies that filesystem events arriving
// while no LSP client is published just drop silently, matching the deliberate
// warm-up behaviour.
func TestLSPFSWatcher_NoClient_NoCrash(t *testing.T) {
	dir := t.TempDir()
	proxy := &clientProxy{} // never .set()
	fw, err := newLSPFSWatcher(dir, proxy)
	if err != nil {
		t.Fatalf("newLSPFSWatcher: %v", err)
	}
	fw.Start()
	defer fw.Stop()

	// Trigger a real event. fswatcher does not synchronously deliver, so allow
	// a short pause; the assertion is "no panic + Stop still returns".
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // cooldown + a margin
}
