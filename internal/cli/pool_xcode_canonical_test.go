package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/xcodebsp"
)

// pool_xcode_canonical_test.go — does the Xcode singleflight key treat two
// spellings of ONE project as one project?
//
// canonicalXcodeRoot is the map key for "one build-server flow per root". It
// used to be Abs + Clean with no symlink resolution, so a checkout reached
// through a symlinked parent keyed differently from the same checkout reached
// directly, and both spellings started their own concurrent xcodebuild flow
// against the same directory.
//
// The assertion is on that EFFECT — one flow, one set of runner calls — and not
// on canonicalXcodeRoot returning equal strings. A string-equality test would
// pass for any implementation that happens to agree with itself, including the
// broken one, which is the point of testing the behaviour the key exists to
// provide.

// TestXcodeSingleflight_SymlinkedSpellingIsOneProject drives the two spellings
// concurrently, the way two sessions attaching to the same project do.
func TestXcodeSingleflight_SymlinkedSpellingIsOneProject(t *testing.T) {
	base := t.TempDir()
	direct := filepath.Join(base, "directproj")
	mustMkdir(t, filepath.Join(direct, "App.xcodeproj"))

	// A parent-directory symlink, which is how this arises in practice: a
	// checkout under a symlinked home, or macOS's /tmp -> /private/tmp firmlink.
	link := filepath.Join(base, "linkproj")
	if err := os.Symlink(direct, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	// Deliberately NOT asserted here. Whether the two spellings produce equal keys
	// is the implementation detail; whether they produce ONE FLOW is the behaviour.
	// Failing on the key would mask the effect assertion below and would pass for
	// any implementation that merely agrees with itself.
	t.Logf("keys: direct=%s link=%s", canonicalXcodeRoot(direct), canonicalXcodeRoot(link))

	runner := &blockingXcodeRunner{root: direct, started: make(chan struct{}), release: make(chan struct{})}
	pool := &workspacePool{
		baseCtx: context.Background(),
		entries: make(map[poolKey]*poolEntry),
		xcode:   poolXcodeState{runner: runner, restart: func(string) error { return nil }},
	}
	cfg := config.XcodeConfig{AutoBuildServer: true, Timeout: config.Duration{Duration: time.Second}}

	// Start via the direct path, wait until it is genuinely in flight, then race the
	// symlinked spelling against it.
	pool.ensureXcodeBuildServer(direct, cfg, true)
	<-runner.started

	var callers sync.WaitGroup
	for range 16 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			pool.ensureXcodeBuildServer(link, cfg, true)
		}()
	}
	callers.Wait()
	close(runner.release)

	waitXcodeState(t, pool, direct, xcodebsp.StateWarming)

	// One flow means exactly the calls one flow makes: the scheme list, then the
	// build-server config. A second flow would double them.
	calls := runner.snapshot()
	if len(calls) != 2 {
		t.Errorf("two spellings of one project ran %d runner calls, want 2 (one flow):\n%#v", len(calls), calls)
	}

	// And both spellings must observe the SAME status, or a caller using the
	// symlinked name sees an empty state for a project that is configured.
	if got := pool.xcodeStatus(link); got.State != xcodebsp.StateWarming {
		t.Errorf("status via the symlinked spelling = %q, want %q — the spellings key apart",
			got.State, xcodebsp.StateWarming)
	}
}

// TestXcodeCanonicalRoot_AnchorsRelativePaths pins the Abs anchor that stays in
// front of paths.Canonical. paths.Canonical returns a relative path unchanged,
// so without the anchor a relative spelling would key apart from the absolute
// one it names.
func TestXcodeCanonicalRoot_AnchorsRelativePaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if got, want := canonicalXcodeRoot("."), canonicalXcodeRoot(dir); got != want {
		t.Errorf("relative and absolute spellings of one directory key apart:\n  %q\n  %q", got, want)
	}
}
