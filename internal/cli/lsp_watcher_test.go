package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/fswatch"
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
		name string
		op   fswatch.Op
		want protocol.FileChangeType
	}{
		{"remove wins", fswatch.Create | fswatch.Remove, protocol.FileDeleted},
		{"create only", fswatch.Create, protocol.FileCreated},
		{"create and write", fswatch.Create | fswatch.Write, protocol.FileCreated},
		{"write only", fswatch.Write, protocol.FileChanged},
		{"rename only", fswatch.Rename, protocol.FileChanged},
		{"chmod only", fswatch.Chmod, protocol.FileChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lspFileChangeType(tc.op); got != tc.want {
				t.Errorf("lspFileChangeType(%v) = %v, want %v", tc.op, got, tc.want)
			}
		})
	}
}

// TestLSPWatchExcludeRegexFor pins the source exclusion against the way it is
// applied (full path AND base name) and against a workspace that lives under a
// dot-prefixed directory. The pattern it replaced was unanchored and matched
// such an ancestor, so the watcher silently delivered nothing for that
// workspace.
func TestLSPWatchExcludeRegexFor(t *testing.T) {
	ws := filepath.Clean(filepath.FromSlash("/home/u/.config/repo"))
	re := regexp.MustCompile(lspWatchExcludeRegexFor(ws))
	excluded := func(rel string) bool {
		p := filepath.Join(ws, filepath.FromSlash(rel))
		return re.MatchString(p) || re.MatchString(filepath.Base(p))
	}
	for _, rel := range []string{"main.go", "src/a/b.go", "src/main.go", ".gitignore", "output.go"} {
		if excluded(rel) {
			t.Errorf("%q excluded at the source; the language server would never hear of it", rel)
		}
	}
	for _, rel := range []string{".plumb/collab.db", ".git/index", "a/.cache/x", "vendor/x.go", "node_modules/p/i.js", "target/debug/x", "out/x", "a/build/o.js", "__pycache__/m.pyc"} {
		if !excluded(rel) {
			t.Errorf("%q not excluded at the source", rel)
		}
	}
}

// TestLSPFSWatcher_DeliversUnderHiddenAncestor drives real filesystem events
// for a workspace below a dot-prefixed directory, the case the unanchored
// exclusion silenced completely.
func TestLSPFSWatcher_DeliversUnderHiddenAncestor(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, ".hidden", "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &watchedFilesRecordingClient{stubClient: &stubClient{}}
	proxy := &clientProxy{}
	proxy.set(rec)
	fw, err := newLSPFSWatcher(ws, proxy)
	if err != nil {
		t.Fatalf("newLSPFSWatcher: %v", err)
	}
	fw.Start()
	t.Cleanup(fw.Stop)
	deadline := time.Now().Add(30 * time.Second)
	for rec.totalEvents() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no event for a workspace under a dot-prefixed directory: the exclusion matched an ancestor")
		}
		if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2*lspWatchCooldown + 100*time.Millisecond)
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
