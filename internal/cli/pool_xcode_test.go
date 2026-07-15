package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
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
