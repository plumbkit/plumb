package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	// The account seam (PLAN-527): with the account authority on, a test binary
	// resolves its global directories from the account database, and the authority
	// lock lives under the account's data directory. Without this a test would name
	// — and, once the authority is taken unconditionally, CONTEND FOR — the
	// developer's real lock, which would make their own `plumb serve` wait out its
	// full start deadline and fail. The seam is inert while the authority is off.
	t.Setenv("PLUMB_TEST_ACCOUNT_ROOT", dir)
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
// it goes to daemon.log while `plumb serve` shows a generic start timeout.
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
	if got := daemonStartTimeoutError("start", short, 30*time.Second).Error(); strings.Contains(got, "sun_path") {
		t.Errorf("a short path must not be blamed on length: %q", got)
	}

	long := "/" + strings.Repeat("a", maxUnixSocketPath) + "/plumb.sock"
	for _, action := range []string{"start", "come back up"} {
		got := daemonStartTimeoutError(action, long, 30*time.Second).Error()
		if !strings.Contains(got, action) || !strings.Contains(got, "30s") {
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

// --- the daemon's two lifetime locks (lock.go) -------------------------------

// unsetInheritedLockEnv makes sure this process sees no inherited lock fd. A test
// binary is not started by a launcher, but the ambient environment must not
// decide the daemon's answer.
func unsetInheritedLockEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{authorityLockFDEnv, daemonLockFDEnv} {
		t.Setenv(name, "")
		os.Unsetenv(name) //nolint:usetesting // must be absent, not empty; t.Setenv above restores it
	}
}

// brokeLifetimeLockPath makes daemonLockPath() unopenable, so any code that
// reaches the runtime lifetime lock reports an open error instead of whatever it
// should have said first. It is how the one order — authority before lifetime —
// is pinned behaviourally rather than by inspection.
func brokeLifetimeLockPath(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(daemonLockPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(daemonLockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptDaemonLock(t *testing.T) {
	withTempRuntime(t)
	t.Run("the held lock file is adopted and made close-on-exec", func(t *testing.T) {
		lock, err := acquireDaemonLock()
		if err != nil {
			t.Fatal(err)
		}
		dup, err := syscall.Dup(int(lock.Fd())) // a fresh fd, inheritable, on the same description
		if err != nil {
			t.Fatal(err)
		}
		adopted, err := adoptDaemonLock(os.NewFile(uintptr(dup), "dup"), daemonLockPath(), inheritedDaemonLockFD, "")
		if err != nil {
			t.Fatal(err)
		}
		defer adopted.Close()
		_ = lock.Close()
		flags, err := fcntlGetFD(int(adopted.Fd()))
		if err != nil || flags&syscall.FD_CLOEXEC == 0 {
			t.Fatalf("the adopted fd must be close-on-exec: flags=%#x err=%v", flags, err)
		}
		if f, err := acquireDaemonLock(); !errors.Is(err, errDaemonAlreadyRunning) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("the adopted lock is not held: %v", err)
		}
	})
	t.Run("another file is refused", func(t *testing.T) {
		other, err := os.CreateTemp(t.TempDir(), "not-the-lock")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adoptDaemonLock(other, daemonLockPath(), inheritedDaemonLockFD, ""); err == nil {
			t.Fatal("adopted a file that is not the lock")
		}
	})
	t.Run("the lock file, but held by someone else, is refused", func(t *testing.T) {
		holder, err := acquireDaemonLock()
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
		f, err := os.OpenFile(daemonLockPath(), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adoptDaemonLock(f, daemonLockPath(), inheritedDaemonLockFD, ""); err == nil {
			t.Fatal("adopted a description that does not hold the lock")
		}
	})
}

// An inherited fd is evidence about a FILE, not about a path: the daemon adopts
// it only when device and inode match the lock it must be. A file of the same
// name somewhere else is the substitution the check exists for, and it must not
// fall back to a fresh acquisition — that is how a substituted path becomes a
// second authority.
func TestHoldDaemonLock_RefusesAnInheritedFdThatIsNotTheLock(t *testing.T) {
	withTempRuntime(t)
	authority, err := authorityPath()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a file of the same name under another root", func(t *testing.T) {
		elsewhere := filepath.Join(t.TempDir(), filepath.Base(authority))
		substitute, err := os.OpenFile(elsewhere, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		fd, err := syscall.Dup(int(substitute.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		_ = substitute.Close()
		t.Setenv("PLUMB_TEST_LOCK_FD", strconv.Itoa(fd))

		_, err = holdDaemonLock(lockHandover{
			env: "PLUMB_TEST_LOCK_FD", path: authority, acquire: acquireAuthority,
		})
		if err == nil {
			t.Fatal("adopted a file that is not the account authority lock")
		}
		if !strings.Contains(err.Error(), authority) {
			t.Errorf("the refusal must name the expected path, got %v", err)
		}
		if _, ok := os.LookupEnv("PLUMB_TEST_LOCK_FD"); ok {
			t.Error("the env var must be unset before the daemon starts children")
		}
		holder, err := acquireAuthority()
		if err != nil {
			t.Fatalf("the refused handover took the authority anyway: %v", err)
		}
		_ = holder.Close()
	})

	// The one mismatch the daemon can name the other path of: the launcher handed
	// the two fds over in the wrong order. A file from anywhere else can only be
	// reported by the expected path and the fd, because a descriptor carries no
	// portable path of its own.
	//
	// Both directions of the swap are pinned. The fd NUMBER is no longer refused
	// on its own — the daemon adopts the fd the env names, whatever it is — so the
	// identity check is now the only thing standing between a swapped pair and a
	// daemon holding the wrong lock.
	t.Run("the other lock of the pair, named", func(t *testing.T) {
		lifetime, err := acquireDaemonLock()
		if err != nil {
			t.Fatal(err)
		}
		defer lifetime.Close()
		fd, err := syscall.Dup(int(lifetime.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("PLUMB_TEST_LOCK_FD", strconv.Itoa(fd))

		_, err = holdDaemonLock(lockHandover{
			env: "PLUMB_TEST_LOCK_FD", path: authority, other: daemonLockPath(), acquire: acquireAuthority,
		})
		if err == nil {
			t.Fatal("adopted the runtime lifetime lock as the account authority")
		}
		for _, want := range []string{authority, daemonLockPath()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal must name both paths; %q lacks %q", err, want)
			}
		}
	})

	t.Run("the pair swapped the other way round", func(t *testing.T) {
		held, err := acquireAuthority()
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		fd, err := syscall.Dup(int(held.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("PLUMB_TEST_LOCK_FD", strconv.Itoa(fd))

		_, err = holdDaemonLock(lockHandover{
			env: "PLUMB_TEST_LOCK_FD", path: daemonLockPath(), other: authority, acquire: acquireDaemonLock,
		})
		if err == nil {
			t.Fatal("adopted the account authority as the runtime lifetime lock")
		}
		for _, want := range []string{daemonLockPath(), authority} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal must name both paths; %q lacks %q", err, want)
			}
		}
	})
}

// A launcher that says it handed a lock over must have handed THAT lock over.
// Anything else — an empty value, rubbish, an fd below the handover floor, an fd
// this process does not have open — refuses to start, and the variable is unset
// before the daemon starts children.
//
// The fd NUMBER is deliberately not one of the refusals: the daemon adopts the
// fd the environment names, because a serve from before the account authority
// hands the lifetime lock over on 3 rather than on the 4 this binary writes (see
// TestDaemonLaunchLocks_AdoptsTheFDOldServesName). What must hold is the FILE
// that fd refers to, which adoptDaemonLock proves and the test above pins.
func TestHoldDaemonLock_RefusesABadInheritedFD(t *testing.T) {
	withTempRuntime(t)
	authority, err := authorityPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		env   string
		path  string
		value string
	}{
		{authorityLockFDEnv, authority, ""},
		{authorityLockFDEnv, authority, "x"},
		{authorityLockFDEnv, authority, strconv.Itoa(minHandoverFD - 1)},
		{authorityLockFDEnv, authority, "969"},
		{daemonLockFDEnv, daemonLockPath(), ""},
		{daemonLockFDEnv, daemonLockPath(), "x"},
		{daemonLockFDEnv, daemonLockPath(), strconv.Itoa(minHandoverFD - 1)},
		{daemonLockFDEnv, daemonLockPath(), "969"},
	} {
		t.Setenv(c.env, c.value)
		if f, err := holdDaemonLock(lockHandover{env: c.env, path: c.path, acquire: acquireDaemonLock}); err == nil {
			_ = f.Close()
			t.Fatalf("%s=%q: started without the lock it names", c.env, c.value)
		}
		if _, ok := os.LookupEnv(c.env); ok {
			t.Fatalf("%s must be unset before the daemon starts children", c.env)
		}
	}
}

// A `plumb daemon` started directly (a service manager, a container CMD) has no
// inherited locks and acquires both itself, in the one order.
func TestDaemonLaunchLocks_WithoutALauncherAcquiresBoth(t *testing.T) {
	withTempRuntime(t)
	unsetInheritedLockEnv(t)
	locks, err := daemonLaunchLocks()
	if err != nil {
		t.Fatalf("a directly started daemon could not take its locks: %v", err)
	}
	defer locks.closeAll()
	if locks.Authority == nil || locks.Lifetime == nil {
		t.Fatalf("the daemon holds %+v, want both locks", locks)
	}
	for name, acquire := range map[string]func() (*os.File, error){
		"account authority": acquireAuthority,
		"lifetime":          acquireDaemonLock,
	} {
		if f, err := acquire(); !errors.Is(err, errDaemonAlreadyRunning) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("the daemon does not hold the %s lock: %v", name, err)
		}
	}
	locks.closeAll()
	freeLaunchLocks(t)
}

// A daemon holding both locks is the singleton: no second daemon may take either
// while it lives, and both are free again the moment it exits.
func TestDaemonLaunchLocks_SecondDaemonRefuses(t *testing.T) {
	withTempRuntime(t)
	unsetInheritedLockEnv(t)
	first, err := daemonLaunchLocks()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemonLaunchLocks(); !errors.Is(err, errDaemonAlreadyRunning) {
		t.Fatalf("a second daemonLaunchLocks = %v, want errDaemonAlreadyRunning", err)
	}
	if _, err := acquireLaunchLocks(); !errors.Is(err, errDaemonAlreadyRunning) {
		t.Fatalf("a launcher took the locks of a running daemon: %v", err)
	}
	first.closeAll()
	second, err := daemonLaunchLocks()
	if err != nil {
		t.Fatalf("the locks were not released when the daemon exited: %v", err)
	}
	second.closeAll()
}

// The one order is the authority first: a refused authority fd is reported
// without the runtime lifetime lock being touched. Pinned behaviourally — the
// lifetime lock path is made unopenable, so a daemon that took it first would
// report that open error instead.
func TestDaemonLaunchLocks_TakesTheAuthorityBeforeTheLifetimeLock(t *testing.T) {
	withTempRuntime(t)
	unsetInheritedLockEnv(t)
	t.Setenv(authorityLockFDEnv, "x")
	brokeLifetimeLockPath(t)

	if _, err := daemonLaunchLocks(); err == nil || !strings.Contains(err.Error(), authorityLockFDEnv) {
		t.Fatalf("daemonLaunchLocks = %v, want the %s refusal", err, authorityLockFDEnv)
	}
}

// launchLocksHelperEnv marks the child process that adopts the handover. It is
// not an environment variable production code reads: the helper below skips in
// every run that does not set it.
const launchLocksHelperEnv = "PLUMB_TEST_LAUNCH_LOCKS_FD3"

// oldServeLifetimeFD is the fd #649's `plumb serve` hands its one lock over on —
// the number an upgraded daemon must accept, and not the 4 this binary writes.
const oldServeLifetimeFD = 3

// TestHelperLaunchLocksFromTheNamedFD is not a test: run as a child process with
// launchLocksHelperEnv set, it holds the runtime lifetime lock on fd 3 — the
// shape a `plumb serve` from before the account authority hands over, with no
// authority named — and reports what daemonLaunchLocks() makes of it.
//
// It is a child process because the fd number is the whole point, and only exec
// can put a named file on fd 3: a test binary's own fds are not ours to move.
func TestHelperLaunchLocksFromTheNamedFD(t *testing.T) {
	if os.Getenv(launchLocksHelperEnv) == "" {
		t.Skip("helper process for the #649 handover shape")
	}
	locks, err := daemonLaunchLocks()
	if err != nil {
		fmt.Println("launchlocks:error:", err)
		return
	}
	defer locks.closeAll()
	if got := locks.Lifetime.Fd(); got != oldServeLifetimeFD {
		fmt.Printf("launchlocks:error: the lifetime lock was adopted on fd %d, want the named fd %d\n",
			got, oldServeLifetimeFD)
		return
	}
	// The authority is not part of the #649 handover, so the daemon takes it
	// fresh — and holds it, or the process this stands in for is not the
	// account's one daemon.
	if f, err := acquireAuthority(); !errors.Is(err, errDaemonAlreadyRunning) {
		if f != nil {
			_ = f.Close()
		}
		fmt.Println("launchlocks:error: the authority is not held after the handover:", err)
		return
	}
	fmt.Printf("launchlocks:ok lifetime_fd=%d\n", oldServeLifetimeFD)
}

// The #649 handover shape, which is what the owner's host runs. #649's
// `plumb serve` had one lock to hand over — the runtime lifetime lock — and wrote
// PLUMB_DAEMON_LOCK_FD=3 for it (oldServeLifetimeFD), naming no authority. A client keeps proxying
// through that older binary until it restarts, and when the daemon is replaced the
// old serve respawns os.Executable() — the NEW binary — with that same
// environment. A daemon that requires the 3/4 pair refuses to start there, and
// every pre-upgrade client loses its daemon.
func TestDaemonLaunchLocks_AdoptsTheFDOldServesName(t *testing.T) {
	withTempRuntime(t)
	unsetInheritedLockEnv(t)
	lifetime, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lifetime.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLaunchLocksFromTheNamedFD$", "-test.count=1")
	cmd.ExtraFiles = []*os.File{lifetime} // fd 3 in the child, exactly as #649 hands it over
	cmd.Env = append(os.Environ(),
		launchLocksHelperEnv+"=1",
		fmt.Sprintf("%s=%d", daemonLockFDEnv, oldServeLifetimeFD),
	)
	out, err := cmd.CombinedOutput()
	if !strings.Contains(string(out), "launchlocks:ok") {
		t.Fatalf("the daemon refused the handover an older serve makes (err %v): %s", err, out)
	}
}

func fcntlGetFD(fd int) (int, error) {
	r, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}
