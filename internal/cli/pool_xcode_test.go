package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/xcodebsp"
)

type staticXcodeTrust bool

func (t staticXcodeTrust) IsTrusted(string) bool { return bool(t) }

type blockingXcodeRunner struct {
	root    string
	started chan struct{}
	release chan struct{}

	mu    sync.Mutex
	once  sync.Once
	calls [][]string
}

func (r *blockingXcodeRunner) Run(_ context.Context, _ string, argv []string, _ time.Duration) (xcodebsp.ExecResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, slices.Clone(argv))
	r.mu.Unlock()

	switch argv[0] {
	case "xcodebuild":
		if r.started != nil {
			r.once.Do(func() { close(r.started) })
		}
		if r.release != nil {
			<-r.release
		}
		return xcodebsp.ExecResult{Stdout: `{"project":{"schemes":["App"]}}`}, nil
	case "xcode-build-server":
		err := os.WriteFile(filepath.Join(r.root, "buildServer.json"), []byte(`{"name":"xcode build server","argv":["xcode-build-server"],"languages":["swift"]}`), 0o644)
		return xcodebsp.ExecResult{}, err
	default:
		return xcodebsp.ExecResult{}, nil
	}
}

func (r *blockingXcodeRunner) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i := range r.calls {
		out[i] = slices.Clone(r.calls[i])
	}
	return out
}

func waitXcodeState(t *testing.T, pool *workspacePool, root string, want xcodebsp.State) xcodebsp.Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := pool.xcodeStatus(root)
		if status.State == want {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Xcode state = %q, want %q", pool.xcodeStatus(root).State, want)
	return xcodebsp.Status{}
}

func TestPoolXcodeSingleflightUsesSafeArgvAndOneRestart(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "App.xcodeproj")
	mustMkdir(t, project)
	runner := &blockingXcodeRunner{
		root: root, started: make(chan struct{}), release: make(chan struct{}),
	}
	var restartMu sync.Mutex
	restarts := 0
	pool := &workspacePool{
		baseCtx: context.Background(),
		entries: make(map[poolKey]*poolEntry),
		xcode: poolXcodeState{
			trust:   staticXcodeTrust(true),
			runner:  runner,
			restart: func(string) error { restartMu.Lock(); restarts++; restartMu.Unlock(); return nil },
		},
	}
	cfg := config.XcodeConfig{AutoBuildServer: true, Timeout: config.Duration{Duration: time.Second}}

	pool.ensureXcodeBuildServer(root, cfg)
	<-runner.started

	var callers sync.WaitGroup
	for i := 0; i < 32; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			pool.ensureXcodeBuildServer(filepath.Join(root, "."), cfg)
		}()
	}
	callers.Wait()
	close(runner.release)

	waitXcodeState(t, pool, root, xcodebsp.StateWarming)
	calls := runner.snapshot()
	if len(calls) != 2 {
		t.Fatalf("runner calls = %#v, want exactly list + config", calls)
	}
	wantList := []string{"xcodebuild", "-project", project, "-list", "-json"}
	wantConfig := []string{"xcode-build-server", "config", "-scheme", "App", "-project", project}
	if !slices.Equal(calls[0], wantList) || !slices.Equal(calls[1], wantConfig) {
		t.Fatalf("runner calls = %#v, want %#v then %#v", calls, wantList, wantConfig)
	}
	for _, call := range calls {
		for _, arg := range call {
			if arg == "build" {
				t.Fatalf("automatic setup must never build the project: %#v", calls)
			}
		}
	}
	restartMu.Lock()
	defer restartMu.Unlock()
	if restarts != 1 {
		t.Fatalf("restart count = %d, want 1", restarts)
	}
}

func TestPoolXcodeNonXcodeWorkspaceExecutesNothing(t *testing.T) {
	root := t.TempDir()
	runner := &blockingXcodeRunner{root: root}
	pool := &workspacePool{
		baseCtx: context.Background(), xcode: poolXcodeState{trust: staticXcodeTrust(true), runner: runner},
	}

	pool.ensureXcodeBuildServer(root, config.XcodeConfig{
		AutoBuildServer: true, Timeout: config.Duration{Duration: time.Second},
	})
	time.Sleep(20 * time.Millisecond)
	if status := pool.xcodeStatus(root); status.State != "" {
		t.Fatalf("non-Xcode workspace state = %q, want no lifecycle state", status.State)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("non-Xcode workspace runner calls = %#v, want none", calls)
	}
}

func TestPoolXcodeDisabledAndUntrustedExecuteNothing(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.XcodeConfig
		trusted bool
		want    xcodebsp.State
	}{
		{name: "disabled", cfg: config.XcodeConfig{}, trusted: true, want: xcodebsp.StateDisabled},
		{name: "untrusted", cfg: config.XcodeConfig{AutoBuildServer: true, Timeout: config.Duration{Duration: time.Second}}, want: xcodebsp.StateUntrusted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			mustMkdir(t, filepath.Join(root, "App.xcodeproj"))
			runner := &blockingXcodeRunner{root: root}
			pool := &workspacePool{
				baseCtx: context.Background(),
				xcode:   poolXcodeState{trust: staticXcodeTrust(tt.trusted), runner: runner},
			}
			pool.ensureXcodeBuildServer(root, tt.cfg)
			waitXcodeState(t, pool, root, tt.want)
			if calls := runner.snapshot(); len(calls) != 0 {
				t.Fatalf("runner calls = %#v, want none", calls)
			}
		})
	}
}

func TestPoolXcodeSemanticProofRequiresValidBuildServer(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "App.xcodeproj"))
	pool := &workspacePool{}
	pool.setXcodeStatus(root, xcodebsp.Status{State: xcodebsp.StateWarming})

	pool.markXcodeSemanticProven(root)
	if got := pool.xcodeStatus(root).State; got != xcodebsp.StateWarming {
		t.Fatalf("state without BSP = %q, want warming", got)
	}

	if err := os.WriteFile(filepath.Join(root, "buildServer.json"), []byte(`{"name":"xcode build server","argv":["xcode-build-server"],"languages":["swift"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pool.markXcodeSemanticProven(root)
	status := pool.xcodeStatus(root)
	if status.State != xcodebsp.StateSemanticProven {
		t.Fatalf("state = %q, want semantic_proven", status.State)
	}
	var decoded xcodebsp.Status
	if err := json.Unmarshal([]byte(pool.xcodeStatusJSON(root)), &decoded); err != nil || decoded.State != xcodebsp.StateSemanticProven {
		t.Fatalf("status JSON = %#v, err = %v", decoded, err)
	}
}

func TestConnXcodeConfigAppliesOnNextSession(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "App.xcodeproj"))
	runner := &blockingXcodeRunner{root: root}
	pool := &workspacePool{
		baseCtx: context.Background(),
		xcode: poolXcodeState{
			trust: staticXcodeTrust(true), runner: runner, restart: func(string) error { return nil },
		},
	}

	current := &connSession{pool: pool}
	current.startXcodeForWorkspace(root, config.XcodeConfig{})
	waitXcodeState(t, pool, root, xcodebsp.StateDisabled)
	current.startXcodeForWorkspace(root, config.XcodeConfig{
		AutoBuildServer: true, Timeout: config.Duration{Duration: time.Second},
	})
	time.Sleep(20 * time.Millisecond)
	if got := pool.xcodeStatus(root).State; got != xcodebsp.StateDisabled {
		t.Fatalf("hot reload changed Xcode state to %q; setting is next-session", got)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("hot reload executed Xcode commands: %#v", calls)
	}

	next := &connSession{pool: pool}
	next.startXcodeForWorkspace(root, config.XcodeConfig{
		AutoBuildServer: true, Timeout: config.Duration{Duration: time.Second},
	})
	waitXcodeState(t, pool, root, xcodebsp.StateWarming)
	if calls := runner.snapshot(); len(calls) != 2 {
		t.Fatalf("next session runner calls = %#v, want list + config", calls)
	}
}

// --- restartSwift late-failure self-heal -----------------------------------
//
// restartSwift wakes an existing hibernated entry rather than building a new
// one (see wakeLocked), so it has the same slow-first-start hazard awaitReady
// self-heals in pool.go: wakeLocked marks the entry poolActive optimistically
// and returns before the woken Supervisor's first OnStart has actually
// completed. If OnStart then fails AFTER restartSwift stops waiting (its
// grace or ctx expiry), nobody is left reading the ready channel unless it is
// handed to reapOnLateStartFailure, and the dead entry (proxy.get() == nil)
// is reused forever. These tests drive that hazard directly against the real
// restartSwift + Supervisor, mirroring lsp.Supervisor's own
// TestSupervisor_StartAsync_SlowOnStart(Failure) pattern: OnStart blocks on a
// test-controlled release channel instead of doing a real LSP handshake.

// swiftEntryWithControllableStart inserts a poolActive "swift" entry backed by
// a real (but never-yet-started) Supervisor over a long-lived no-op process
// (sleepCommand). Its OnStart hook signals startedCh (if non-nil) the instant
// it is entered, then blocks on release before returning *outcome — letting a
// test deterministically control both WHEN the woken start resolves and
// whether it succeeds or fails. Mirrors the insertWarming helper in
// pool_selfheal_test.go, but for restartSwift's wake path rather than a fresh
// acquire.
func swiftEntryWithControllableStart(t *testing.T, p *workspacePool, root string, startedCh chan<- struct{}, release <-chan struct{}, outcome *error) *poolEntry {
	t.Helper()
	cmd, args := sleepCommand(t)
	var once sync.Once
	sup := lsp.NewSupervisor(cmd, args, nil, lsp.SupervisorOptions{
		OnStart: func(context.Context, *jsonrpc.Conn) error {
			if startedCh != nil {
				once.Do(func() { close(startedCh) })
			}
			<-release
			return *outcome
		},
	})
	e := &poolEntry{
		root: root, language: "swift", proxy: &clientProxy{}, sup: sup,
		state: poolActive, startedAt: time.Now(),
	}
	p.mu.Lock()
	p.entries[poolKey{root, "swift"}] = e
	p.mu.Unlock()
	return e
}

// waitSwiftEntryGone polls until root's "swift" entry is evicted or the
// deadline passes.
func waitSwiftEntryGone(t *testing.T, p *workspacePool, root string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if p.lookup(root, "swift") == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("swift entry for %s still present after %s; late-failure self-heal did not evict it", root, within)
}

// runRestartSwift runs restartSwift on a goroutine (it blocks for up to the
// pool's grace) and returns its result, failing the test if it does not
// return within timeout.
func runRestartSwift(t *testing.T, p *workspacePool, root string, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.restartSwift(root) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatal("restartSwift did not return in time")
		return nil
	}
}

// TestRestartSwift_GraceExpiry_LateFailureEvictsAndSelfHeals is the core
// regression test for the woken-entry counterpart of
// TestAwaitReady_SlowFailureEvictsAndSelfHeals: a woken restart that fails
// AFTER restartSwift's grace window must still evict the dead entry, and the
// NEXT acquire for the same (root, "swift") must build a genuinely fresh
// entry rather than reusing the corpse.
func TestRestartSwift_GraceExpiry_LateFailureEvictsAndSelfHeals(t *testing.T) {
	pool := &workspacePool{
		entries:    make(map[poolKey]*poolEntry),
		baseCtx:    context.Background(),
		cacheTTL:   time.Minute,
		startGrace: 15 * time.Millisecond,
	}
	defer pool.close()

	root := t.TempDir()
	release := make(chan struct{})
	var outcome error
	dead := swiftEntryWithControllableStart(t, pool, root, nil, release, &outcome)

	// restartSwift hibernates + wakes dead, then bails at the grace with the
	// woken Supervisor's OnStart still blocked on release.
	if err := runRestartSwift(t, pool, root, 2*time.Second); err != nil {
		t.Fatalf("restartSwift returned an error at the grace: %v", err)
	}
	if pool.lookup(root, "swift") != dead {
		t.Fatal("entry vanished before the late failure arrived")
	}

	// The woken start now fails, slowly — after restartSwift already returned.
	outcome = errors.New("initialize: connection closed")
	close(release)

	// Self-heal 1: the drain evicts the dead entry.
	waitSwiftEntryGone(t, pool, root, time.Second)

	// Self-heal 2: the next acquire builds a FRESH entry instead of reusing the
	// dead one — proving the pool recovered rather than caching a corpse.
	cmd, args := sleepCommand(t)
	pool.langs = []langConfig{{name: "swift", cfg: config.LSPConfig{Command: cmd, Args: args, Enabled: true}}}
	fresh, err := pool.acquireLang(context.Background(), root, "swift", false)
	if err != nil {
		t.Fatalf("acquire after eviction: %v", err)
	}
	if fresh == nil {
		t.Fatal("acquire after eviction returned nil entry")
	}
	if fresh == dead {
		t.Fatal("acquire after eviction reused the dead entry; self-heal failed")
	}
}

// TestRestartSwift_GraceExpiry_LateSuccessNotEvicted is the false-positive
// guard: a woken start that succeeds AFTER the grace (a slow but healthy
// restart) must NOT be evicted by the drain.
func TestRestartSwift_GraceExpiry_LateSuccessNotEvicted(t *testing.T) {
	pool := &workspacePool{
		entries:    make(map[poolKey]*poolEntry),
		baseCtx:    context.Background(),
		cacheTTL:   time.Minute,
		startGrace: 15 * time.Millisecond,
	}
	defer pool.close()

	root := t.TempDir()
	release := make(chan struct{})
	var outcome error // stays nil: a late SUCCESS
	e := swiftEntryWithControllableStart(t, pool, root, nil, release, &outcome)

	if err := runRestartSwift(t, pool, root, 2*time.Second); err != nil {
		t.Fatalf("restartSwift returned an error at the grace: %v", err)
	}

	close(release) // the woken start now succeeds, slowly

	// Give the drain ample time to (wrongly) act, then assert the entry survives.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if pool.lookup(root, "swift") != e {
			t.Fatal("a healthy slow-start entry was wrongly evicted by the drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRestartSwift_CtxDone_LateFailureEvicts covers the SECOND abandon
// branch: restartSwift waits on the pool's base (daemon-lifetime) context, not
// a per-request one, but the same late-failure leak applies if that context is
// cancelled (daemon shutdown) while the woken start is still in flight. A
// startedCh handshake ensures the base ctx is cancelled only once the woken
// Supervisor's OnStart has actually begun blocking on release — cancelling
// the shared base ctx any earlier would race the fast spawn-failure path
// instead of exercising the ctx.Done() branch this test targets.
func TestRestartSwift_CtxDone_LateFailureEvicts(t *testing.T) {
	baseCtx, cancel := context.WithCancel(context.Background())
	pool := &workspacePool{
		entries:    make(map[poolKey]*poolEntry),
		baseCtx:    baseCtx,
		cacheTTL:   time.Minute,
		startGrace: time.Hour, // irrelevant — the ctx branch must win
	}
	defer pool.close()

	root := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	var outcome error
	dead := swiftEntryWithControllableStart(t, pool, root, started, release, &outcome)

	done := make(chan error, 1)
	go func() { done <- pool.restartSwift(root) }()

	select {
	case <-started:
		// The woken Supervisor's OnStart is now blocked on release; safe to cancel
		// the base ctx without racing the fast spawn-failure path.
	case <-time.After(2 * time.Second):
		t.Fatal("woken Supervisor's OnStart never started")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("restartSwift on a cancelled base ctx returned nil, want ctx.Err()")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restartSwift did not return on a cancelled base ctx")
	}
	if pool.lookup(root, "swift") != dead {
		t.Fatal("entry vanished before the late failure arrived")
	}

	// The woken start now fails, slowly — after restartSwift already returned on
	// the cancelled ctx.
	outcome = errors.New("initialize: connection closed after ctx cancel")
	close(release)
	waitSwiftEntryGone(t, pool, root, time.Second)
}
