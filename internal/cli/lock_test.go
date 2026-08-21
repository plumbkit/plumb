package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// withTempRuntime redirects plumbRuntimeDir() so the lock files land in a
// t.TempDir() and don't collide with the user's real runtime dir or with other
// tests running in parallel.
func withTempRuntime(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// XDG_RUNTIME_DIR is cleared first and deliberately: it now takes priority
	// over the cache dir, so on a real Linux desktop the developer's own
	// /run/user/$UID would win and these tests would write their locks into it.
	t.Setenv("XDG_RUNTIME_DIR", "")
	// The fallback is os.UserCacheDir, which honours XDG_CACHE_HOME on Linux and
	// HOME on macOS. Setting both is the portable way to redirect it.
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
}

func TestAcquireDaemonLock_SecondAttemptFails(t *testing.T) {
	withTempRuntime(t)

	first, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("first acquireDaemonLock: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	_, err = acquireDaemonLock()
	if !errors.Is(err, errDaemonAlreadyRunning) {
		t.Fatalf("second acquireDaemonLock: want errDaemonAlreadyRunning, got %v", err)
	}
}

// TestAcquireDaemonLock_ReleasesOnClose simulates the crash-recovery story:
// closing the fd (which is what the kernel does when a process dies) must
// make the lock immediately reacquirable by another caller.
func TestAcquireDaemonLock_ReleasesOnClose(t *testing.T) {
	withTempRuntime(t)

	first, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("first acquireDaemonLock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing first lock: %v", err)
	}

	second, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("second acquireDaemonLock after release: %v", err)
	}
	_ = second.Close()
}

// TestAcquireSpawnLock_SerialisesWaiters verifies the serve-side spawn lock
// blocks concurrent callers and serves them one at a time. This is the
// property that prevents the "two daemons spawned simultaneously" bug.
func TestAcquireSpawnLock_SerialisesWaiters(t *testing.T) {
	withTempRuntime(t)

	first, err := acquireSpawnLock(context.Background())
	if err != nil {
		t.Fatalf("first acquireSpawnLock: %v", err)
	}

	got := make(chan time.Time, 1)
	go func() {
		f, err := acquireSpawnLock(context.Background())
		if err != nil {
			t.Errorf("second acquireSpawnLock: %v", err)
			close(got)
			return
		}
		got <- time.Now()
		_ = f.Close()
	}()

	// Give the goroutine time to enter the flock retry loop.
	time.Sleep(150 * time.Millisecond)
	release := time.Now()
	_ = first.Close()

	select {
	case acquiredAt, ok := <-got:
		if !ok {
			t.Fatal("second acquireSpawnLock failed")
		}
		if acquiredAt.Before(release) {
			t.Fatalf("second lock acquired %v before first released — not serialised",
				release.Sub(acquiredAt))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second acquireSpawnLock did not return within 2s after release")
	}
}

func TestAcquireSpawnLock_CancellationHonoured(t *testing.T) {
	withTempRuntime(t)

	holder, err := acquireSpawnLock(context.Background())
	if err != nil {
		t.Fatalf("holding lock: %v", err)
	}
	defer holder.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = acquireSpawnLock(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("cancellation took %v — should be ~100ms", elapsed)
	}
}

// TestAcquireDaemonLock_ParallelStress runs 20 goroutines all trying to acquire
// the daemon lock. Only one should ever hold it at a time. Regression test
// for the original "two daemons race" bug.
func TestAcquireDaemonLock_ParallelStress(t *testing.T) {
	withTempRuntime(t)

	const N = 20
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		holderCount int
		maxHolders  int
		successes   int
	)

	for range N {
		wg.Go(func() {
			f, err := acquireDaemonLock()
			if err != nil {
				if !errors.Is(err, errDaemonAlreadyRunning) {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			mu.Lock()
			holderCount++
			if holderCount > maxHolders {
				maxHolders = holderCount
			}
			successes++
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			holderCount--
			mu.Unlock()
			_ = f.Close()
		})
	}
	wg.Wait()

	if maxHolders > 1 {
		t.Fatalf("up to %d goroutines held the lock simultaneously — should be 1", maxHolders)
	}
	if successes == 0 {
		t.Fatal("no goroutine acquired the lock")
	}
	// We don't assert successes == 1: closing-then-reopening between goroutines
	// can let later attempts succeed. The invariant is that no two hold it at once.
}

// confirm plumbRuntimeDir uses the temp HOME we set.
func TestLockPaths_RespectUserCacheDir(t *testing.T) {
	withTempRuntime(t)
	got := plumbRuntimeDir()
	cache, _ := os.UserCacheDir()
	want := filepath.Join(cache, "plumb")
	if got != want {
		t.Fatalf("plumbRuntimeDir = %q, want %q", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("plumbRuntimeDir must create the directory the locks live in: %v", err)
	}
	if filepath.Dir(spawnLockPath()) != got {
		t.Fatalf("spawnLockPath not under runtime dir: %s", spawnLockPath())
	}
	if filepath.Dir(daemonLockPath()) != got {
		t.Fatalf("daemonLockPath not under runtime dir: %s", daemonLockPath())
	}
}

// An over-long socket path is rejected by bind() as EINVAL — "invalid
// argument" — which says nothing about length, and the daemon's only report of
// it goes to daemon.log while `plumb serve` shows a generic 10-second timeout.
// Reproduced on Linux with a long XDG_CACHE_HOME.
func TestSocketPathLengthHint(t *testing.T) {
	if got := socketPathLengthHint("/home/u/.cache/plumb/plumb.sock"); got != "" {
		t.Errorf("a short path needs no hint, got %q", got)
	}

	long := "/" + strings.Repeat("a", maxUnixSocketPath) + "/plumb.sock"
	got := socketPathLengthHint(long)
	if got == "" {
		t.Fatal("an over-long path must be explained")
	}
	if !strings.Contains(got, "sun_path") || !strings.Contains(got, socketPathShortenLever()) {
		t.Errorf("the hint must name the cause and the lever, got %q", got)
	}
	if !strings.Contains(got, strconv.Itoa(len(long))) {
		t.Errorf("the hint must state the actual length, got %q", got)
	}
}

// The lever is whichever variable actually moves the socket on this host, and
// that is not one answer: $XDG_RUNTIME_DIR when the runtime dir is in use,
// $HOME on darwin (os.UserCacheDir ignores XDG_CACHE_HOME there), and
// XDG_CACHE_HOME otherwise. Naming the wrong one sends the user to a setting
// that cannot move the socket — which is the bug this replaced.
func TestSocketPathShortenLever_TracksWhereTheSocketActuallyIs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	t.Setenv("XDG_RUNTIME_DIR", "")
	wantFallback := "XDG_CACHE_HOME"
	if runtime.GOOS == "darwin" {
		wantFallback = "$HOME"
	}
	if got := socketPathShortenLever(); got != wantFallback {
		t.Errorf("with no runtime dir, lever = %q, want %q", got, wantFallback)
	}

	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return
	}
	run := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(run, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", run)
	if got := socketPathShortenLever(); got != "XDG_RUNTIME_DIR" {
		t.Errorf("with the socket under the runtime dir, lever = %q, want XDG_RUNTIME_DIR", got)
	}
}

// legacyDaemonSocketPath is for DIAGNOSIS only — naming the other directory
// when a daemon is running there. It must be empty when there is no other
// directory, or the warning fires against the socket we are already using.
func TestLegacyDaemonSocketPath(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("the runtime dir does not move on this platform")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := legacyDaemonSocketPath(); got != "" {
		t.Errorf("legacyDaemonSocketPath = %q, want empty when it IS the current dir", got)
	}

	run := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(run, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", run)

	cache, _ := os.UserCacheDir()
	if got, want := legacyDaemonSocketPath(), filepath.Join(cache, "plumb", "plumb.sock"); got != want {
		t.Errorf("legacyDaemonSocketPath = %q, want %q", got, want)
	}
	// And the socket actually in use stays the runtime-dir one — the legacy
	// path is never something we connect to.
	if got, want := daemonSocketPath(), filepath.Join(run, "plumb", "plumb.sock"); got != want {
		t.Errorf("daemonSocketPath = %q, want %q", got, want)
	}
}

// Every daemon-facing path must come from ONE directory. A process that dialled
// one directory's socket while reading another's version file or control socket
// was the half-migrated state this pins against.
func TestDaemonPaths_AllShareOneRuntimeDir(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("the runtime dir does not move on this platform")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	run := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(run, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", run)

	want := filepath.Join(run, "plumb")
	for name, got := range map[string]string{
		"socket":      daemonSocketPath(),
		"ctrl socket": daemonCtrlSocketPath(),
		"pid":         daemonPIDPath(),
		"version":     daemonVersionPath(),
		"spawn lock":  spawnLockPath(),
		"daemon lock": daemonLockPath(),
	} {
		if filepath.Dir(got) != want {
			t.Errorf("%s is in %q, want %q — all daemon paths must share one runtime dir", name, filepath.Dir(got), want)
		}
	}
}

// The hint has to ride the error the USER sees. The daemon's own listen error
// goes to daemon.log and then the daemon exits, so for anyone running plumb
// through an MCP client the start timeout is the only report there is — which
// is why both callers go through daemonStartTimeoutError.
func TestDaemonStartTimeoutError_CarriesTheLengthHint(t *testing.T) {
	short := "/home/u/.cache/plumb/plumb.sock"
	if got := daemonStartTimeoutError("start", short).Error(); strings.Contains(got, "sun_path") {
		t.Errorf("a short path must not be blamed on length: %q", got)
	}

	long := "/" + strings.Repeat("a", maxUnixSocketPath) + "/plumb.sock"
	for _, action := range []string{"start", "come back up"} {
		got := daemonStartTimeoutError(action, long).Error()
		if !strings.Contains(got, action) {
			t.Errorf("%q: the message must name what did not happen, got %q", action, got)
		}
		if !strings.Contains(got, "sun_path") || !strings.Contains(got, socketPathShortenLever()) {
			t.Errorf("%q: the timeout must carry the length hint, got %q", action, got)
		}
	}
}

// sun_path is 104 bytes on macOS/BSD *including* the NUL, so 104 usable bytes
// already fails to bind and must still be explained.
func TestSocketPathLengthHint_BoundaryIsPortable(t *testing.T) {
	if maxUnixSocketPath != 103 {
		t.Fatalf("maxUnixSocketPath = %d, want 103 (104-byte sun_path less the NUL)", maxUnixSocketPath)
	}
	atLimit := strings.Repeat("a", maxUnixSocketPath)
	if got := socketPathLengthHint(atLimit); got != "" {
		t.Errorf("a path exactly at the limit is fine, got %q", got)
	}
	if got := socketPathLengthHint(atLimit + "a"); got == "" {
		t.Error("one byte over the limit must be explained")
	}
}
