package cli

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/session"
)

// TestConnRegistry_ReloadProject_OnlyMatchingWorkspace verifies the per-workspace
// reload targets only the sessions pinned to that workspace (trailing-slash
// tolerant) and never touches a session in another project.
func TestConnRegistry_ReloadProject_OnlyMatchingWorkspace(t *testing.T) {
	r := newConnRegistry()
	var aCount, bCount int
	r.add("a", connHandle{workspace: func() string { return "/repoA" }, reloadProject: func() { aCount++ }})
	r.add("a2", connHandle{workspace: func() string { return "/repoA/" }, reloadProject: func() { aCount++ }})
	r.add("b", connHandle{workspace: func() string { return "/repoB" }, reloadProject: func() { bCount++ }})

	r.reloadProject("/repoA")
	if aCount != 2 {
		t.Errorf("repoA sessions reloaded %d times, want 2 (trailing slash tolerant)", aCount)
	}
	if bCount != 0 {
		t.Errorf("repoB must not reload on a repoA change; got %d", bCount)
	}
}

func TestServerWriteTimeout(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "unset uses default", want: mcp.DefaultWriteTimeout},
		{name: "duration override", env: "2s", want: 2 * time.Second},
		{name: "trimmed duration override", env: " 1500ms ", want: 1500 * time.Millisecond},
		{name: "zero disables", env: "0", want: 0},
		{name: "off disables", env: "off", want: 0},
		{name: "disable disables", env: "disable", want: 0},
		{name: "disabled disables", env: "disabled", want: 0},
		{name: "none disables", env: "none", want: 0},
		{name: "bad value uses default", env: "not-a-duration", want: mcp.DefaultWriteTimeout},
		{name: "negative value uses default", env: "-1s", want: mcp.DefaultWriteTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == "" {
				t.Setenv("PLUMB_WRITE_TIMEOUT", "")
				_ = os.Unsetenv("PLUMB_WRITE_TIMEOUT")
			} else {
				t.Setenv("PLUMB_WRITE_TIMEOUT", tt.env)
			}
			if got := serverWriteTimeout(); got != tt.want {
				t.Errorf("serverWriteTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServerToolExecTimeout(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "unset uses default", want: mcp.DefaultToolExecTimeout},
		{name: "duration override", env: "5s", want: 5 * time.Second},
		{name: "trimmed duration override", env: " 250ms ", want: 250 * time.Millisecond},
		{name: "zero disables", env: "0", want: 0},
		{name: "off disables", env: "off", want: 0},
		{name: "disable disables", env: "disable", want: 0},
		{name: "disabled disables", env: "disabled", want: 0},
		{name: "none disables", env: "none", want: 0},
		{name: "bad value uses default", env: "not-a-duration", want: mcp.DefaultToolExecTimeout},
		{name: "negative value uses default", env: "-1s", want: mcp.DefaultToolExecTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == "" {
				t.Setenv("PLUMB_TOOL_EXEC_TIMEOUT", "")
				_ = os.Unsetenv("PLUMB_TOOL_EXEC_TIMEOUT")
			} else {
				t.Setenv("PLUMB_TOOL_EXEC_TIMEOUT", tt.env)
			}
			if got := serverToolExecTimeout(); got != tt.want {
				t.Errorf("serverToolExecTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSeedPathFromArgs(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string
	}{
		{
			name: "uri with file scheme",
			args: `{"uri":"file:///tmp/foo.go"}`,
			want: "/tmp/foo.go",
		},
		{
			name: "uri without scheme",
			args: `{"uri":"/tmp/foo.go"}`,
			want: "/tmp/foo.go",
		},
		{
			name: "path field",
			args: `{"path":"/tmp/foo.go"}`,
			want: "/tmp/foo.go",
		},
		{
			name: "root field (list_files)",
			args: `{"root":"/tmp/proj"}`,
			want: "/tmp/proj",
		},
		{
			name: "workspace field (session_start)",
			args: `{"workspace":"/tmp/proj"}`,
			want: "/tmp/proj",
		},
		{
			name: "paths array (read_multiple_files)",
			args: `{"paths":["/tmp/a.go","/tmp/b.go"]}`,
			want: "/tmp/a.go",
		},
		{
			name: "operations array (transaction_apply)",
			args: `{"operations":[{"path":"/tmp/a.go","edits":[]},{"path":"/tmp/b.go","edits":[]}]}`,
			want: "/tmp/a.go",
		},
		{
			name: "uri preferred over path",
			args: `{"uri":"file:///tmp/from-uri","path":"/tmp/from-path"}`,
			want: "/tmp/from-uri",
		},
		{
			name: "path preferred over paths array",
			args: `{"path":"/tmp/single","paths":["/tmp/multi"]}`,
			want: "/tmp/single",
		},
		{
			name: "empty paths array falls through",
			args: `{"paths":[]}`,
			want: "",
		},
		{
			name: "empty operations array falls through",
			args: `{"operations":[]}`,
			want: "",
		},
		{
			name: "no recognised field",
			args: `{"unrelated":"value"}`,
			want: "",
		},
		{
			name: "malformed JSON",
			args: `{not-json`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := seedPathFromArgs(json.RawMessage(tt.args))
			if got != tt.want {
				t.Errorf("seedPathFromArgs(%s) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// TestWorkspaceFromArgs_NestedShapes guards against the regression where
// transaction_apply and read_multiple_files were invisible to workspace
// attribution because their paths are nested. Without this, stats for these
// tools fall back to the connection's primary workspace and are dropped for
// sessions that never resolved one.
func TestWorkspaceFromArgs_NestedShapes(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "go.mod"), "module test\n")
	pool := detectTestPool()

	cases := []struct {
		name string
		args string
	}{
		{
			name: "transaction_apply with one operation",
			args: `{"operations":[{"path":"` + dir + `/sub/foo.go","edits":[]}]}`,
		},
		{
			name: "transaction_apply with multiple operations",
			args: `{"operations":[{"path":"` + dir + `/sub/foo.go","edits":[]},{"path":"` + dir + `/bar.go","edits":[]}]}`,
		},
		{
			name: "read_multiple_files",
			args: `{"paths":["` + dir + `/sub/foo.go","` + dir + `/bar.go"]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := workspaceFromArgs(pool, json.RawMessage(tc.args))
			if got != dir {
				t.Errorf("workspaceFromArgs = %q, want %q", got, dir)
			}
		})
	}
}

func TestDaemonScansTxlogSynchronouslyOnAttach(t *testing.T) {
	called := make(chan struct{})
	release := make(chan struct{})
	scan := func(string) {
		close(called)
		<-release
	}

	done := make(chan struct{})
	go func() {
		recoverWorkspaceTxlog("/workspace", scan)
		close(done)
	}()

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("scanTxlog was not called")
	}

	select {
	case <-done:
		t.Fatal("recoverWorkspaceTxlog returned before txlog scan completed")
	default:
	}

	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recoverWorkspaceTxlog did not return after txlog scan completed")
	}
}

// TestDetectAndSynthesiseRoot_GitTreeAgree verifies that for a git repo with
// no language marker, Detect now resolves to the .git-bearing root as
// LanguageNone (the fix for the "stuck on resolving" bug), and that
// SynthesiseRoot — still used by the auto_attach path for non-git trees —
// agrees on the same root.
func TestDetectAndSynthesiseRoot_GitTreeAgree(t *testing.T) {
	root := freshTempDir(t)
	mustMkdir(t, filepath.Join(root, ".git"))
	sub := filepath.Join(root, "pkg", "myapp")
	mustMkdir(t, sub)

	pool := detectTestPool()

	// Detect resolves the git root directly, even without a language marker.
	gotRoot, lang, err := pool.Detect(sub)
	if err != nil {
		t.Fatalf("Detect: unexpected error %v — a .git ancestor should resolve", err)
	}
	if gotRoot != root {
		t.Errorf("Detect root = %q, want %q (.git ancestor)", gotRoot, root)
	}
	if lang != LanguageNone {
		t.Errorf("Detect language = %q, want %q", lang, LanguageNone)
	}

	// SynthesiseRoot walks up to the same .git-bearing ancestor.
	if got := pool.SynthesiseRoot(sub); got != root {
		t.Errorf("SynthesiseRoot = %q, want %q (.git ancestor)", got, root)
	}
}

// TestAutoAttach_SynthesiseRootNoGit verifies that when no .git exists anywhere
// in the tree, SynthesiseRoot falls back to the seed directory itself.
func TestAutoAttach_SynthesiseRootNoGit(t *testing.T) {
	root := freshTempDir(t)
	sub := filepath.Join(root, "a", "b", "c")
	mustMkdir(t, sub)

	pool := detectTestPool()
	got := pool.SynthesiseRoot(sub)
	if got != sub {
		t.Errorf("SynthesiseRoot = %q, want seed %q", got, sub)
	}
}

// TestAutoAttach_MaterialisePlumbDir verifies that materialisePlumbDir creates
// <root>/.plumb/ and is idempotent (calling twice does not error).
func TestAutoAttach_MaterialisePlumbDir(t *testing.T) {
	root := freshTempDir(t)

	if err := materialisePlumbDir(root); err != nil {
		t.Fatalf("first call: %v", err)
	}
	plumbDir := filepath.Join(root, ".plumb")
	if info, err := os.Stat(plumbDir); err != nil || !info.IsDir() {
		t.Fatalf(".plumb/ not created at %s", plumbDir)
	}

	// Idempotent — must not fail when the directory already exists.
	if err := materialisePlumbDir(root); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}
}

// TestAutoAttach_MaterialisePlumbDir_NestedRoot verifies creation works when
// intermediate directories in the synthetic root path do not yet exist.
func TestAutoAttach_MaterialisePlumbDir_NestedRoot(t *testing.T) {
	root := freshTempDir(t)
	nested := filepath.Join(root, "a", "b", "c")
	// Do NOT create the nested directories — materialisePlumbDir must do it.
	if err := materialisePlumbDir(nested); err != nil {
		t.Fatalf("materialisePlumbDir: %v", err)
	}
	plumbDir := filepath.Join(nested, ".plumb")
	if info, err := os.Stat(plumbDir); err != nil || !info.IsDir() {
		t.Fatalf(".plumb/ not created under nested path %s", plumbDir)
	}
}

// TestReapAfterExit verifies the daemon-spawn reaper waits on the child (so it
// is reaped rather than left a zombie) and closes the spawner's copy of the log
// handle. Run synchronously here so the read of ProcessState is race-free.
func TestReapAfterExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX helper command")
	}
	logFile, err := os.Create(filepath.Join(t.TempDir(), "child.log"))
	if err != nil {
		t.Fatalf("create log: %v", err)
	}

	cmd := exec.Command("true")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper: %v", err)
	}

	reapAfterExit(cmd, logFile)

	if cmd.ProcessState == nil {
		t.Fatal("child was not reaped (ProcessState nil after reapAfterExit)")
	}
	if !cmd.ProcessState.Exited() {
		t.Errorf("child did not exit cleanly: %v", cmd.ProcessState)
	}
	if _, err := logFile.WriteString("x"); err == nil {
		t.Error("spawner's log handle should be closed after reapAfterExit")
	}
}

// TestArmShutdownWatchdog_ForcesExitAfterDeadline verifies the watchdog forces
// process exit (code 0) a bounded time after the daemon context is cancelled by
// a shutdown signal, and not before.
func TestArmShutdownWatchdog_ForcesExitAfterDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan int, 1)
	armShutdownWatchdog(ctx, 50*time.Millisecond, func(code int) { exited <- code })

	// No signal yet: the watchdog must stay dormant.
	select {
	case <-exited:
		t.Fatal("watchdog fired before the context was cancelled")
	case <-time.After(80 * time.Millisecond):
	}

	cancel() // simulate SIGTERM cancelling the daemon context
	select {
	case code := <-exited:
		if code != 0 {
			t.Errorf("watchdog exit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("watchdog did not force exit within the deadline after cancellation")
	}
}

// TestArmShutdownWatchdog_NoExitWhileServing verifies a daemon that never
// receives a shutdown signal is never force-exited.
func TestArmShutdownWatchdog_NoExitWhileServing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan int, 1)
	armShutdownWatchdog(ctx, 50*time.Millisecond, func(code int) { exited <- code })

	select {
	case <-exited:
		t.Fatal("watchdog fired without a shutdown signal")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestDrainConnections covers both the clean drain and the wedged-connection
// timeout the accept loop relies on at shutdown.
func TestDrainConnections(t *testing.T) {
	t.Run("drains before the deadline", func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			time.Sleep(20 * time.Millisecond)
			wg.Done()
		}()
		if !drainConnections(&wg, time.Second) {
			t.Error("reported a timeout for a group that drained in time")
		}
	})

	t.Run("times out on a wedged connection", func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(1) // never Done — a wedged in-flight request
		if drainConnections(&wg, 50*time.Millisecond) {
			t.Error("reported success for a group that never drained")
		}
		wg.Done() // release the internal waiter goroutine
	})
}

// TestNewConnSession_ContextWiring guards the idle-eviction fix: the session
// context must be a child of the daemon context (so a daemon-wide shutdown
// cancels it) AND s.cancel() must cancel it (so the idle reaper's cancel makes
// mcp.Serve return and the deferred Unregister run). The original bug derived
// s.ctx from context.Background(), so the reaper's cancel reached a context
// nothing observed and idle eviction was a silent no-op.
func TestNewConnSession_ContextWiring(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	pool := detectTestPool()

	t.Run("daemon shutdown cancels the session", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())
		s := newConnSession(parent, pool, nil, store, nil, nil, newSharedBudgets())
		defer s.close()
		cancelParent() // daemon-wide shutdown
		select {
		case <-s.ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("session context not cancelled when the daemon context was cancelled")
		}
	})

	t.Run("idle eviction cancels the session", func(t *testing.T) {
		s := newConnSession(context.Background(), pool, nil, store, nil, nil, newSharedBudgets())
		defer s.close()
		s.cancel() // exactly what connRegistry.evictIdle invokes via the registry
		select {
		case <-s.ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("session context not cancelled by s.cancel(); idle eviction would not terminate the connection")
		}
	})
}

func TestIdleReaperEvictsLiveConnection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfg := config.Defaults()
	cfg.Session.EvictionTTLMinutes = 1
	store := config.NewStore(cfg)
	pool := detectTestPool()
	registry := newConnRegistry()
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		handleConn(context.Background(), serverConn, pool, nil, nil, nil, store, nil, nil, time.Now(), newSharedBudgets(), registry)
		close(done)
	}()

	var sessID string
	deadline := time.After(time.Second)
	for sessID == "" {
		registry.mu.Lock()
		for id := range registry.conns {
			sessID = id
			break
		}
		registry.mu.Unlock()
		if sessID != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("connection was not registered")
		case <-time.After(10 * time.Millisecond):
		}
	}

	dir, err := session.Dir()
	if err != nil {
		t.Fatalf("session dir: %v", err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(filepath.Join(dir, sessID+".json"), old, old); err != nil {
		t.Fatalf("age session file: %v", err)
	}

	reaperCtx, cancelReaper := context.WithCancel(context.Background())
	defer cancelReaper()
	ticks := make(chan time.Time)
	go runIdleReaper(reaperCtx, store, registry, nil, nil, ticks)
	ticks <- time.Now()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle reaper did not make handleConn return")
	}
	active, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("idle-evicted session still listed active: %#v", active)
	}
}

// TestShutdownHardDeadlineExceedsInnerGraces guards that the shutdown watchdog
// stays a genuine last resort: it must allow the sequential bounded teardown
// (connection drain + pool.close handshake) to finish with headroom for the
// still-unbounded topology/supervisor stops, so it never force-exits — and
// truncates a topology resync — during a slow-but-normal shutdown.
func TestShutdownHardDeadlineExceedsInnerGraces(t *testing.T) {
	if shutdownHardDeadline <= acceptDrainGrace+poolCloseGrace {
		t.Fatalf("shutdownHardDeadline (%s) must exceed acceptDrainGrace+poolCloseGrace (%s) so the watchdog does not trip during a slow-but-normal shutdown",
			shutdownHardDeadline, acceptDrainGrace+poolCloseGrace)
	}
}

func TestDaemonStopWaitExceedsShutdownHardDeadline(t *testing.T) {
	if daemonStopWait <= shutdownHardDeadline {
		t.Fatalf("daemonStopWait (%s) must exceed shutdownHardDeadline (%s)", daemonStopWait, shutdownHardDeadline)
	}
}

// TestConnSession_LoggerCarriesSessionID verifies that per-connection log
// records emitted via s.logger carry a session_id attribute, while
// daemon-global records (which still use package-level slog) do not.
// This test must NOT be run in parallel — it mutates slog.Default().
func TestConnSession_LoggerCarriesSessionID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	type record struct {
		msg   string
		attrs map[string]string
	}
	var mu sync.Mutex
	var records []record

	h := &captureHandler{fn: func(msg string, attrs map[string]string) {
		mu.Lock()
		records = append(records, record{msg: msg, attrs: attrs})
		mu.Unlock()
	}}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prev)

	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	defer s.close()

	// Emit one record via the session logger (session-scoped).
	s.logger.Info("test session-scoped record")
	// Emit one record via the global logger (daemon-global, no session_id).
	slog.Info("test global record")

	mu.Lock()
	defer mu.Unlock()

	var sessionRecord, globalRecord *record
	for i := range records {
		switch records[i].msg {
		case "test session-scoped record":
			sessionRecord = &records[i]
		case "test global record":
			globalRecord = &records[i]
		}
	}
	if sessionRecord == nil {
		t.Fatal("session-scoped record not emitted")
	}
	if globalRecord == nil {
		t.Fatal("global record not emitted")
	}
	if id := sessionRecord.attrs["session_id"]; id == "" {
		t.Error("session-scoped record missing session_id attribute")
	}
	if id := globalRecord.attrs["session_id"]; id != "" {
		t.Errorf("global record should not carry session_id, got %q", id)
	}
}

// captureHandler is a minimal slog.Handler that calls fn for every record.
type captureHandler struct {
	fn    func(msg string, attrs map[string]string)
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &captureHandler{fn: h.fn, attrs: append(h.attrs, attrs...)}
	return next
}
func (h *captureHandler) WithGroup(_ string) slog.Handler { return h }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	m := make(map[string]string)
	for _, a := range h.attrs {
		m[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	h.fn(r.Message, m)
	return nil
}
