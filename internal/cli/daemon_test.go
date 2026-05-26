package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

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
