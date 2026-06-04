package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/config"
)

// blockingShutdownClient embeds stubClient (routing_proxy_test.go) and makes
// Shutdown block until its context is cancelled, simulating a cold or hung
// language server during daemon teardown.
type blockingShutdownClient struct {
	*stubClient
	entered chan struct{}
}

func (b *blockingShutdownClient) Shutdown(ctx context.Context) error {
	close(b.entered)
	<-ctx.Done()
	return ctx.Err()
}

// TestCloseEntry_BoundsHungShutdown verifies closeEntry returns once its bounded
// context fires even when the language server's Shutdown never responds — so a
// hung LSP cannot stall daemon exit.
func TestCloseEntry_BoundsHungShutdown(t *testing.T) {
	client := &blockingShutdownClient{stubClient: &stubClient{}, entered: make(chan struct{})}
	e := &poolEntry{proxy: &clientProxy{}}
	e.proxy.set(client)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		closeEntry(ctx, e)
		close(done)
	}()

	select {
	case <-client.entered:
	case <-time.After(time.Second):
		t.Fatal("Shutdown was never called")
	}
	select {
	case <-done:
		// returned once the deadline fired — the hung Shutdown did not stall close
	case <-time.After(2 * time.Second):
		t.Fatal("closeEntry did not return after its deadline; a hung Shutdown stalls daemon exit")
	}
}

// detectTestPool builds a workspacePool with Go and Python enabled, matching
// the default plumb configuration. Used by all Detect tests below.
func detectTestPool() *workspacePool {
	return &workspacePool{
		entries:  make(map[string]*poolEntry),
		baseCtx:  context.Background(),
		cacheTTL: time.Minute, // mirror production: a zero TTL would panic cache.New's ticker
		langs: []langConfig{
			{name: "go", cfg: config.LSPConfig{
				RootMarkers: []string{"go.mod"},
				Enabled:     true,
			}},
			{name: "python", cfg: config.LSPConfig{
				RootMarkers: []string{"pyproject.toml", "setup.py"},
				Enabled:     true,
			}},
		},
	}
}

// freshTempDir creates a temp directory under the OS-level $TMPDIR rather
// than via t.TempDir(). Reason: pool.Detect walks up to the filesystem root
// looking for ancestral markers. The plumb repo uses GOTMPDIR=.testcache to
// keep test binaries inside the project (Airlock Digital workaround), and
// go test's t.TempDir() honours GOTMPDIR — so a t.TempDir() lands inside the
// plumb source tree, and Detect finds plumb's own go.mod as an ancestor
// marker. Going via os.MkdirTemp("", …) bypasses GOTMPDIR and lands under
// /var/folders (macOS) / /tmp (Linux) where no Go module exists above.
func freshTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "plumb-detect-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestDetect_LanguageMarkerOnly(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "go.mod"), "module test\n")

	pool := detectTestPool()
	root, lang, err := pool.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if root != dir {
		t.Errorf("root: got %s, want %s", root, dir)
	}
	if lang != "go" {
		t.Errorf("language: got %s, want go", lang)
	}
}

func TestDetect_PlumbMarkerWithLanguage(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, ".plumb"))
	mustWrite(t, filepath.Join(dir, "go.mod"), "module test\n")

	pool := detectTestPool()
	root, lang, err := pool.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if root != dir {
		t.Errorf("root: got %s, want %s", root, dir)
	}
	if lang != "go" {
		t.Errorf("language: got %s, want go", lang)
	}
}

// TestDetect_PlumbMarkerWithoutLanguage is the regression test for the
// "TUI stuck on resolving" bug. A .plumb/ marker in a non-Go/non-Python
// project (e.g. a JavaScript repo) must still resolve so filesystem tools,
// stats attribution, and project config all keep working.
func TestDetect_PlumbMarkerWithoutLanguage(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, ".plumb"))
	mustWrite(t, filepath.Join(dir, "package.json"), "{}")

	pool := detectTestPool()
	root, lang, err := pool.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: unexpected error %v — .plumb/ alone should resolve as LanguageNone", err)
	}
	if root != dir {
		t.Errorf("root: got %s, want %s", root, dir)
	}
	if lang != LanguageNone {
		t.Errorf("language: got %s, want %s", lang, LanguageNone)
	}
}

func TestDetect_PlumbInParentNoLanguage(t *testing.T) {
	root := freshTempDir(t)
	sub := filepath.Join(root, "sub", "deep")
	mustMkdir(t, sub)
	mustMkdir(t, filepath.Join(root, ".plumb"))

	pool := detectTestPool()
	gotRoot, lang, err := pool.Detect(sub)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if gotRoot != root {
		t.Errorf("root: got %s, want %s", gotRoot, root)
	}
	if lang != LanguageNone {
		t.Errorf("language: got %s, want %s", lang, LanguageNone)
	}
}

// TestDetect_PlumbInChildWinsOverGoModInParent verifies the documented
// priority order: a `.plumb/` marker always beats a language marker found
// only in an ancestor, even when the ancestor has Go.
func TestDetect_PlumbInChildWinsOverGoModInParent(t *testing.T) {
	parent := freshTempDir(t)
	mustWrite(t, filepath.Join(parent, "go.mod"), "module test\n")
	child := filepath.Join(parent, "sub")
	mustMkdir(t, child)
	mustMkdir(t, filepath.Join(child, ".plumb"))

	pool := detectTestPool()
	gotRoot, lang, err := pool.Detect(child)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if gotRoot != child {
		t.Errorf("root: got %s, want %s (child should win)", gotRoot, child)
	}
	// Language detection at child walks up to parent and finds go.mod.
	if lang != "go" {
		t.Errorf("language: got %s, want go (go.mod is in ancestor)", lang)
	}
}

func TestDetect_NothingFound(t *testing.T) {
	// Use a fresh tmpdir tree with no markers anywhere up to the FS root.
	// Strictly speaking the FS root could have a marker; in practice TempDir
	// is somewhere under /tmp or /var/folders, neither of which has a go.mod
	// or .plumb on any normal dev machine. If this test ever flakes, the
	// machine has bigger problems.
	dir := freshTempDir(t)

	pool := detectTestPool()
	_, _, err := pool.Detect(dir)
	if err == nil {
		t.Fatal("Detect: want error, got nil")
	}
}

// TestDetect_GitDirOnly is the regression test for the "TUI stuck on
// resolving" bug on git repos with no language marker (scripts /
// multi-language repos). A bare .git/ must resolve as LanguageNone so the
// session attaches in the default config (auto_attach off).
func TestDetect_GitDirOnly(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, ".git"))

	pool := detectTestPool()
	root, lang, err := pool.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: unexpected error %v — .git/ alone should resolve as LanguageNone", err)
	}
	if root != dir {
		t.Errorf("root: got %s, want %s", root, dir)
	}
	if lang != LanguageNone {
		t.Errorf("language: got %s, want %s", lang, LanguageNone)
	}
}

// TestDetect_GitDirInAncestor mirrors the reported layout: a deep script
// directory whose only project boundary is a .git/ several levels up.
func TestDetect_GitDirInAncestor(t *testing.T) {
	root := freshTempDir(t)
	mustMkdir(t, filepath.Join(root, ".git"))
	sub := filepath.Join(root, "alerts", "runbook", "check-entra-app-secret")
	mustMkdir(t, sub)

	pool := detectTestPool()
	gotRoot, lang, err := pool.Detect(sub)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if gotRoot != root {
		t.Errorf("root: got %s, want %s (git root)", gotRoot, root)
	}
	if lang != LanguageNone {
		t.Errorf("language: got %s, want %s", lang, LanguageNone)
	}
}

// TestDetect_LanguageMarkerWinsOverGit verifies precedence: a language marker
// at the same directory as .git resolves as that language, not LanguageNone.
func TestDetect_LanguageMarkerWinsOverGit(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, ".git"))
	mustWrite(t, filepath.Join(dir, "go.mod"), "module test\n")

	pool := detectTestPool()
	_, lang, err := pool.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if lang != "go" {
		t.Errorf("language: got %s, want go (go.mod must beat .git)", lang)
	}
}

// TestDetect_NearestLanguageMarkerWinsOverAncestorGit verifies that a language
// marker in a subdirectory beats a .git/ in an ancestor — the walk returns at
// the nearer directory.
func TestDetect_NearestLanguageMarkerWinsOverAncestorGit(t *testing.T) {
	root := freshTempDir(t)
	mustMkdir(t, filepath.Join(root, ".git"))
	sub := filepath.Join(root, "services", "api")
	mustMkdir(t, sub)
	mustWrite(t, filepath.Join(sub, "go.mod"), "module api\n")

	pool := detectTestPool()
	gotRoot, lang, err := pool.Detect(sub)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if gotRoot != sub {
		t.Errorf("root: got %s, want %s (subproject should win)", gotRoot, sub)
	}
	if lang != "go" {
		t.Errorf("language: got %s, want go", lang)
	}
}

// TestDetect_GitAtHomeRefused verifies the $HOME guard: a dotfiles repo at the
// home directory must not turn all of $HOME into a workspace. Detect should
// walk past it and (finding nothing else) error.
func TestDetect_GitAtHomeRefused(t *testing.T) {
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	mustMkdir(t, filepath.Join(home, ".git"))
	sub := filepath.Join(home, "notes")
	mustMkdir(t, sub)

	pool := detectTestPool()
	if _, _, err := pool.Detect(sub); err == nil {
		t.Fatal("Detect: want error (a .git at $HOME must not resolve), got nil")
	}
}

// TestDetect_GitAtHomeTrailingSlashRefused guards the $HOME exclusion against a
// non-canonical spelling: a raw string compare against os.UserHomeDir() would
// not match "$HOME/" and would wrongly resolve $HOME as a workspace. The
// identity-based guard must still refuse it.
func TestDetect_GitAtHomeTrailingSlashRefused(t *testing.T) {
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	mustMkdir(t, filepath.Join(home, ".git"))

	pool := detectTestPool()
	if _, _, err := pool.Detect(home + string(filepath.Separator)); err == nil {
		t.Fatal("Detect: want error (a trailing-slash spelling of $HOME must not resolve), got nil")
	}
}

// TestDetect_GitAtHomeViaSymlinkRefused guards the $HOME exclusion against a
// symlink alias of the home directory: os.SameFile resolves it to $HOME, so the
// .git there must still be refused — a string compare would not match the
// symlink path.
func TestDetect_GitAtHomeViaSymlinkRefused(t *testing.T) {
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	mustMkdir(t, filepath.Join(home, ".git"))
	alias := filepath.Join(freshTempDir(t), "home-alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	pool := detectTestPool()
	if _, _, err := pool.Detect(alias); err == nil {
		t.Fatal("Detect: want error (a symlink alias of $HOME must not resolve), got nil")
	}
}

func TestSynthesiseRoot_GitDirAtSeed(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, ".git"))

	pool := detectTestPool()
	got := pool.SynthesiseRoot(dir)
	if got != dir {
		t.Errorf("SynthesiseRoot: got %s, want %s", got, dir)
	}
}

func TestSynthesiseRoot_GitDirInAncestor(t *testing.T) {
	root := freshTempDir(t)
	mustMkdir(t, filepath.Join(root, ".git"))
	sub := filepath.Join(root, "pkg", "foo")
	mustMkdir(t, sub)

	pool := detectTestPool()
	got := pool.SynthesiseRoot(sub)
	if got != root {
		t.Errorf("SynthesiseRoot: got %s, want %s", got, root)
	}
}

// TestSynthesiseRoot_NoGitFallsBackToSeed verifies the fallback: when no .git/
// exists anywhere up the tree, the seed directory itself is returned.
func TestSynthesiseRoot_NoGitFallsBackToSeed(t *testing.T) {
	// Build a subtree under os.MkdirTemp to stay away from any .git above.
	base := freshTempDir(t)
	sub := filepath.Join(base, "a", "b")
	mustMkdir(t, sub)

	pool := detectTestPool()
	got := pool.SynthesiseRoot(sub)
	// The seed is sub; no .git anywhere above it in the temp tree.
	if got != sub {
		t.Errorf("SynthesiseRoot: got %s, want %s (seed fallback)", got, sub)
	}
}

func TestCurrentEnvPATH(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"present", []string{"HOME=/root", "PATH=/usr/bin:/bin", "USER=x"}, "/usr/bin:/bin"},
		{"absent", []string{"HOME=/root", "USER=x"}, ""},
		{"empty value", []string{"PATH="}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := currentEnvPATH(tc.env); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAugmentedPATH_PreservesExistingOrder(t *testing.T) {
	current := "/custom/bin:/usr/bin:/bin"
	result := augmentedPATH(current)
	entries := filepath.SplitList(result)
	if len(entries) < 3 {
		t.Fatalf("expected at least 3 entries, got %d: %v", len(entries), entries)
	}
	if entries[0] != "/custom/bin" {
		t.Errorf("first entry: got %q, want /custom/bin", entries[0])
	}
	if entries[1] != "/usr/bin" {
		t.Errorf("second entry: got %q, want /usr/bin", entries[1])
	}
}

func TestAugmentedPATH_DeduplicatesEntries(t *testing.T) {
	current := "/usr/local/bin:/usr/bin"
	result := augmentedPATH(current)
	entries := filepath.SplitList(result)
	seen := make(map[string]int)
	for _, e := range entries {
		seen[e]++
	}
	for p, count := range seen {
		if count > 1 {
			t.Errorf("duplicate entry %q appears %d times", p, count)
		}
	}
}

func TestAugmentedPATH_AppendsHomebrewPaths(t *testing.T) {
	// Start from a PATH that lacks Homebrew dirs entirely.
	result := augmentedPATH("/usr/bin:/bin")
	entries := filepath.SplitList(result)
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		set[e] = true
	}
	for _, want := range []string{"/usr/local/bin", "/opt/homebrew/bin"} {
		if !set[want] {
			t.Errorf("expected %q in augmented PATH, got %v", want, entries)
		}
	}
}

func TestAugmentedPATH_EmptyInput(t *testing.T) {
	result := augmentedPATH("")
	if result == "" {
		t.Error("augmentedPATH(\"\") returned empty string; expected at least Homebrew paths")
	}
	if !strings.Contains(result, "/usr/local/bin") {
		t.Errorf("expected /usr/local/bin in result, got %q", result)
	}
}

func TestEnvFor_AlwaysSetsPATH(t *testing.T) {
	cfg := config.LSPConfig{} // no overrides
	env := envFor(cfg)
	if env == nil {
		t.Fatal("envFor returned nil; expected explicit env slice with PATH set")
	}
	path := currentEnvPATH(env)
	if path == "" {
		t.Error("PATH not set in envFor result")
	}
	if !strings.Contains(path, "/usr/local/bin") {
		t.Errorf("PATH does not contain /usr/local/bin: %q", path)
	}
}

func TestEnvFor_ConfigOverrideWins(t *testing.T) {
	cfg := config.LSPConfig{
		Env: map[string]string{"PATH": "/my/custom/bin"},
	}
	env := envFor(cfg)
	path := currentEnvPATH(env)
	if path != "/my/custom/bin" {
		t.Errorf("PATH: got %q, want /my/custom/bin", path)
	}
}

// mustWrite writes data to path, creating parent dirs as needed.
func mustWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// missingPoolCommand is a path that cannot exist, so the language-server spawn
// fails immediately.
const missingPoolCommand = "/nonexistent/plumb-pool-test-binary"

// sleepCommand returns a long-lived no-op command, so acquireLang spawns a real
// process that never completes the LSP handshake — the entry stays "warming".
func sleepCommand(t *testing.T) (string, []string) {
	t.Helper()
	path, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no 'sleep' binary on PATH")
	}
	return path, []string{"30"}
}

// warmingPool builds a pool whose "go" language server is the given command.
func warmingPool(baseCtx context.Context, command string, args []string) *workspacePool {
	return &workspacePool{
		entries:  make(map[string]*poolEntry),
		baseCtx:  baseCtx,
		cacheTTL: 5 * time.Minute,
		langs: []langConfig{
			{name: "go", cfg: config.LSPConfig{
				Command:     command,
				Args:        args,
				RootMarkers: []string{"go.mod"},
				Enabled:     true,
			}},
		},
	}
}

// TestAcquireLang_ConcurrentReusesSingleEntry verifies that concurrent acquires
// for the same cold root reuse one warming entry and never spawn a second
// language server — the entry is published into the map before the pool lock is
// released, so racing callers all observe it.
func TestAcquireLang_ConcurrentReusesSingleEntry(t *testing.T) {
	cmd, args := sleepCommand(t)
	pool := warmingPool(context.Background(), cmd, args)
	defer pool.close()

	const root = "/tmp/plumb-acquire-concurrent-root"
	const n = 8
	var wg sync.WaitGroup
	results := make([]*poolEntry, n)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, err := pool.acquireLang(context.Background(), root, "go", false)
			if err != nil {
				t.Errorf("acquireLang: %v", err)
				return
			}
			results[i] = e
		}(i)
	}
	wg.Wait()

	first := results[0]
	if first == nil {
		t.Fatal("nil entry from acquireLang")
	}
	for i, e := range results {
		if e != first {
			t.Fatalf("result[%d] differs from result[0]; concurrent acquire spawned more than one LS", i)
		}
	}
	if got := len(pool.entries); got != 1 {
		t.Fatalf("pool has %d entries, want 1", got)
	}
	// Still warming: the handshake never completed against the no-op process.
	if first.proxy.get() != nil {
		t.Fatal("expected a not-yet-ready proxy for the no-op language server")
	}
}

// TestAcquireLang_FirstStartFailureRemovesEntry verifies that a spawn failure
// (missing binary) returns fast, surfaces the error, and removes the dead entry
// so a later acquire can retry — preserving the missing-binary degrade path.
func TestAcquireLang_FirstStartFailureRemovesEntry(t *testing.T) {
	pool := warmingPool(context.Background(), missingPoolCommand, nil)
	defer pool.close()

	const root = "/tmp/plumb-acquire-fail-root"
	start := time.Now()
	if _, err := pool.acquireLang(context.Background(), root, "go", false); err == nil {
		t.Fatal("expected error for a missing language-server binary")
	}
	if elapsed := time.Since(start); elapsed >= firstStartGrace {
		t.Fatalf("acquireLang blocked %s on a fast spawn failure; want well under the %s grace", elapsed, firstStartGrace)
	}
	if e := pool.lookup(root); e != nil {
		t.Fatal("failed entry was not removed; a later acquire cannot retry")
	}
}

// TestAcquireLang_CancelledCtxKeepsWarming is the regression test for the
// latent lifetime bug: the supervisor runs on the pool's base context, not the
// caller's request ctx, so a cancelled request returns the warming entry
// immediately and leaves the language server warming in the pool.
func TestAcquireLang_CancelledCtxKeepsWarming(t *testing.T) {
	cmd, args := sleepCommand(t)
	pool := warmingPool(context.Background(), cmd, args)
	defer pool.close()

	const root = "/tmp/plumb-acquire-cancel-root"
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // request context already gone before the call

	start := time.Now()
	e, err := pool.acquireLang(ctx, root, "go", false)
	if err != nil {
		t.Fatalf("acquireLang: %v", err)
	}
	if e == nil {
		t.Fatal("nil entry from acquireLang")
	}
	if elapsed := time.Since(start); elapsed >= firstStartGrace {
		t.Fatalf("acquireLang waited %s despite a cancelled request ctx", elapsed)
	}
	if got := pool.lookup(root); got != e {
		t.Fatal("entry missing after cancelled-ctx acquire; supervisor lifetime leaked to the request ctx")
	}
	if e.sup == nil {
		t.Fatal("expected a live supervisor on the warming entry")
	}
}

// poolRefs returns the pinned refcount for root under the pool lock, or -1 when
// no entry exists. Race-safe for assertions in the refcount tests.
func poolRefs(p *workspacePool, root string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[root]; ok {
		return e.refs
	}
	return -1
}

// waitEntryGone polls until root's entry is reclaimed or the deadline passes.
// Teardown is asynchronous (a time.AfterFunc grace timer), so the assertion
// must poll rather than read once.
func waitEntryGone(t *testing.T, p *workspacePool, root string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if p.lookup(root) == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("entry for %s still present after %s; idle teardown did not fire", root, within)
}

// fastWarmingPool builds a warming pool with a short idle grace and returns it
// alongside a pre-cancelled context, so a pinned acquire of a never-ready
// (sleep) server returns immediately instead of waiting out firstStartGrace.
func fastWarmingPool(t *testing.T, grace time.Duration) (*workspacePool, context.Context) {
	t.Helper()
	cmd, args := sleepCommand(t)
	pool := warmingPool(context.Background(), cmd, args)
	pool.idleGrace = grace
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return pool, ctx
}

// TestPool_RefcountKeepsSharedEntryAlive verifies the shared-workspace contract:
// two sessions pinning the same root share one entry (refs=2), and a single
// session leaving does not tear down the server the other still uses.
func TestPool_RefcountKeepsSharedEntryAlive(t *testing.T) {
	pool, ctx := fastWarmingPool(t, 20*time.Millisecond)
	defer pool.close()

	const root = "/tmp/plumb-refcount-shared-root"
	if _, err := pool.acquireLang(ctx, root, "go", true); err != nil {
		t.Fatalf("first pinned acquire: %v", err)
	}
	if _, err := pool.acquireLang(ctx, root, "go", true); err != nil {
		t.Fatalf("second pinned acquire: %v", err)
	}
	if got := poolRefs(pool, root); got != 2 {
		t.Fatalf("refs = %d, want 2 after two pinned acquires", got)
	}

	pool.release(root) // one session leaves; the other still holds the entry
	if got := poolRefs(pool, root); got != 1 {
		t.Fatalf("refs = %d, want 1 after one release", got)
	}
	time.Sleep(3 * pool.idleGrace)
	if pool.lookup(root) == nil {
		t.Fatal("entry torn down while a session still holds it (refs > 0)")
	}
}

// TestPool_GraceTeardownAfterLastSession verifies the last-leaver reclaim: once
// the final pinned reference is released, the language server is torn down after
// the idle grace.
func TestPool_GraceTeardownAfterLastSession(t *testing.T) {
	pool, ctx := fastWarmingPool(t, 20*time.Millisecond)
	defer pool.close()

	const root = "/tmp/plumb-grace-teardown-root"
	if _, err := pool.acquireLang(ctx, root, "go", true); err != nil {
		t.Fatalf("pinned acquire: %v", err)
	}
	pool.release(root) // last (only) session leaves -> schedule idle teardown
	waitEntryGone(t, pool, root, 2*time.Second)
}

// TestPool_PinDuringGraceCancelsTeardown verifies that a re-attach inside the
// grace window cancels the pending teardown (the Claude Desktop
// disconnect-then-reconnect case) so the warm server is reused, not killed.
func TestPool_PinDuringGraceCancelsTeardown(t *testing.T) {
	pool, ctx := fastWarmingPool(t, 200*time.Millisecond)
	defer pool.close()

	const root = "/tmp/plumb-grace-cancel-root"
	if _, err := pool.acquireLang(ctx, root, "go", true); err != nil {
		t.Fatalf("pinned acquire: %v", err)
	}
	pool.release(root) // schedule teardown after the grace window
	time.Sleep(30 * time.Millisecond)
	if _, err := pool.acquireLang(ctx, root, "go", true); err != nil { // re-pin before grace fires
		t.Fatalf("re-pin acquire: %v", err)
	}
	time.Sleep(3 * pool.idleGrace)
	if pool.lookup(root) == nil {
		t.Fatal("entry torn down despite a re-pin during the grace window")
	}
	if got := poolRefs(pool, root); got != 1 {
		t.Fatalf("refs = %d, want 1 after release + re-pin", got)
	}
}

// TestPool_UnpinnedAcquireNeverScheduledForTeardown verifies that an on-demand
// routing acquire (pin=false) holds no reference and is never reclaimed by the
// refcount path — release is a no-op for a never-pinned entry, matching the
// pre-refcount "lives until shutdown" behaviour.
func TestPool_UnpinnedAcquireNeverScheduledForTeardown(t *testing.T) {
	pool, ctx := fastWarmingPool(t, 20*time.Millisecond)
	defer pool.close()

	const root = "/tmp/plumb-unpinned-root"
	if _, err := pool.acquireLang(ctx, root, "go", false); err != nil {
		t.Fatalf("unpinned acquire: %v", err)
	}
	if got := poolRefs(pool, root); got != 0 {
		t.Fatalf("refs = %d, want 0 for an unpinned acquire", got)
	}
	pool.release(root) // defensive no-op: refs already 0, must not schedule teardown
	time.Sleep(3 * pool.idleGrace)
	if pool.lookup(root) == nil {
		t.Fatal("unpinned entry was reclaimed; release must be a no-op when refs == 0")
	}
}
