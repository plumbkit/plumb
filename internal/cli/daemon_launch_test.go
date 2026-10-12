package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// testLauncher is a launcher on the real lifetime lock (in a temp runtime dir)
// whose dial succeeds once ready reports true. Its start counts, and keeps a
// duplicate of the lock open until the test ends: a stand-in for a live child
// that inherited it.
func testLauncher(t *testing.T, ready *atomic.Bool, starts *atomic.Int32, wait time.Duration) daemonLauncher {
	t.Helper()
	return daemonLauncher{
		dial: func() (net.Conn, error) {
			if ready.Load() {
				a, b := net.Pipe()
				t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
				return a, nil
			}
			return nil, errors.New("connection refused")
		},
		acquire: acquireDaemonLock,
		start: func(lock *os.File) error {
			starts.Add(1)
			child, err := syscall.Dup(int(lock.Fd()))
			if err != nil {
				return err
			}
			t.Cleanup(func() { _ = syscall.Close(child) })
			return nil
		},
		wait: wait,
		poll: 10 * time.Millisecond,
	}
}

func TestDialOrLaunch_ARunningDaemonIsDialledWithoutTheLock(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	ready.Store(true)
	l := testLauncher(t, &ready, &starts, time.Second)
	acquired := false
	l.acquire = func() (*os.File, error) { acquired = true; return acquireDaemonLock() }
	conn, launched, err := l.dialOrLaunch(context.Background())
	if err != nil || conn == nil || launched {
		t.Fatalf("got conn=%v launched=%v err=%v, want a dial and no launch", conn, launched, err)
	}
	if acquired || starts.Load() != 0 {
		t.Fatalf("a daemon that answers must not be launched beside: acquired=%v starts=%d", acquired, starts.Load())
	}
}

// The race PLAN-528 fixes: a daemon is starting (it holds the lock) but does
// not accept yet. The launcher must never start a second one; it waits and,
// at its deadline, fails naming the silent holder.
func TestDialOrLaunch_NeverStartsBesideAHolder(t *testing.T) {
	withTempRuntime(t)
	holder, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, 300*time.Millisecond)
	_, launched, err := l.dialOrLaunch(context.Background())
	if !errors.Is(err, errDaemonHeldButSilent) || launched {
		t.Fatalf("got launched=%v err=%v, want errDaemonHeldButSilent", launched, err)
	}
	if starts.Load() != 0 {
		t.Fatalf("started %d daemon(s) beside the holder", starts.Load())
	}
}

// A starting daemon that comes up within the deadline is simply joined.
func TestDialOrLaunch_JoinsAHolderThatComesUp(t *testing.T) {
	withTempRuntime(t)
	holder, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, 5*time.Second)
	time.AfterFunc(150*time.Millisecond, func() { ready.Store(true) })
	conn, launched, err := l.dialOrLaunch(context.Background())
	if err != nil || conn == nil || launched || starts.Load() != 0 {
		t.Fatalf("got conn=%v launched=%v err=%v starts=%d, want to join without starting", conn, launched, err, starts.Load())
	}
}

// `plumb restart`: the stopped daemon still holds the lock while it exits.
// The launcher waits for the release, then starts exactly one.
func TestDialOrLaunch_StartsOnceAfterTheHolderExits(t *testing.T) {
	withTempRuntime(t)
	holder, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, 5*time.Second)
	inner := l.start
	l.start = func(lock *os.File) error {
		// The launcher holds the authority while it starts the child.
		if other, err := acquireDaemonLock(); !errors.Is(err, errDaemonAlreadyRunning) {
			if other != nil {
				_ = other.Close()
			}
			t.Errorf("start ran without holding the lock: %v", err)
		}
		ready.Store(true)
		return inner(lock)
	}
	time.AfterFunc(150*time.Millisecond, func() { _ = holder.Close() })
	conn, launched, err := l.dialOrLaunch(context.Background())
	if err != nil || conn == nil || !launched || starts.Load() != 1 {
		t.Fatalf("got conn=%v launched=%v err=%v starts=%d, want exactly one launch", conn, launched, err, starts.Load())
	}
}

// A daemon that never answers is launched once, not again on every poll.
func TestDialOrLaunch_LaunchesAtMostOnce(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, 200*time.Millisecond)
	_, launched, err := l.dialOrLaunch(context.Background())
	if !errors.Is(err, errDaemonStartTimeout) || !launched || starts.Load() != 1 {
		t.Fatalf("got launched=%v err=%v starts=%d, want one launch then a timeout", launched, err, starts.Load())
	}
}

// A daemon this call started that exits before accepting (a bad config, an
// over-long socket path) is reported at once, not after the whole wait.
func TestDialOrLaunch_ReportsADaemonThatExitedEarly(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, 5*time.Second)
	l.start = func(*os.File) error { starts.Add(1); return nil } // no child keeps the lock
	began := time.Now()
	_, launched, err := l.dialOrLaunch(context.Background())
	if !errors.Is(err, errDaemonExitedEarly) || !launched || starts.Load() != 1 {
		t.Fatalf("got launched=%v err=%v starts=%d, want errDaemonExitedEarly after one launch", launched, err, starts.Load())
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("took %s: an early exit must not wait out the deadline", took)
	}
}

// Held, then released, then launched, then silent: a timeout of the daemon this
// call started, not the held-by-another error.
func TestDialOrLaunch_HeldThenLaunchedThenSilentIsAStartTimeout(t *testing.T) {
	withTempRuntime(t)
	holder, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, 500*time.Millisecond)
	time.AfterFunc(100*time.Millisecond, func() { _ = holder.Close() })
	_, launched, err := l.dialOrLaunch(context.Background())
	if !errors.Is(err, errDaemonStartTimeout) || !launched || starts.Load() != 1 {
		t.Fatalf("got launched=%v err=%v starts=%d, want errDaemonStartTimeout after one launch", launched, err, starts.Load())
	}
}

func TestDialOrLaunch_HonoursCancellation(t *testing.T) {
	withTempRuntime(t)
	holder, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	began := time.Now()
	if _, _, err := l.dialOrLaunch(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("took %s after cancellation", took)
	}
}

func TestDialOrLaunch_ACancelledCallStartsNothing(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := l.dialOrLaunch(ctx); !errors.Is(err, context.Canceled) || starts.Load() != 0 {
		t.Fatalf("got err=%v starts=%d, want a cancelled call to start nothing", err, starts.Load())
	}
	f, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("a cancelled call kept the lock: %v", err)
	}
	_ = f.Close()
}

func TestDialOrLaunch_ReturnsALockError(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, time.Second)
	boom := errors.New("opening daemon lock: permission denied")
	l.acquire = func() (*os.File, error) { return nil, boom }
	if _, _, err := l.dialOrLaunch(context.Background()); !errors.Is(err, boom) || starts.Load() != 0 {
		t.Fatalf("got err=%v starts=%d, want the lock error and no start", err, starts.Load())
	}
}

// The launcher must not keep its copy of the lock: a long-lived serve would
// otherwise hold the authority itself and block its own later reconnect.
func TestDialOrLaunch_ClosesItsCopyOfTheLock(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, time.Second)
	var passed *os.File
	inner := l.start
	l.start = func(lock *os.File) error { passed = lock; ready.Store(true); return inner(lock) }
	if _, _, err := l.dialOrLaunch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := passed.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("the launcher's copy of the lock is still open (Stat: %v)", err)
	}
}

func TestDialOrLaunch_DecidesOnce(t *testing.T) {
	withTempRuntime(t)
	holder, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, 300*time.Millisecond)
	var decided, held int
	l.decided = func() { decided++ }
	l.onHeld = func() { held++ }
	_, _, _ = l.dialOrLaunch(context.Background())
	if decided != 1 || held != 1 {
		t.Fatalf("decided=%d onHeld=%d over many polls, want each exactly once", decided, held)
	}
}

func TestLaunchError(t *testing.T) {
	withTempRuntime(t)
	sock := "/tmp/plumb/plumb.sock"
	for _, c := range []struct {
		err  error
		want []string
	}{
		{errDaemonStartTimeout, []string{"did not start within 30s", sock}},
		{context.DeadlineExceeded, []string{"did not start within 30s"}},
		{errDaemonHeldButSilent, []string{"holds", daemonLockPath(), "within 30s", daemonLogPath(), "plumb stop"}},
		{errDaemonExitedEarly, []string{"exited before accepting", daemonLogPath()}},
	} {
		got := launchError("start", sock, 30*time.Second, c.err).Error()
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%v: %q lacks %q", c.err, got, w)
			}
		}
	}
	other := errors.New("other")
	if got := launchError("start", sock, time.Second, other); !errors.Is(got, other) || got.Error() != other.Error() {
		t.Errorf("an unrelated error must pass through, got %v", got)
	}
}

func TestDialOrLaunch_AFailedStartReleasesTheLock(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, time.Second)
	l.start = func(*os.File) error { return errors.New("exec failed") }
	if _, _, err := l.dialOrLaunch(context.Background()); err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Fatalf("got %v, want the start error", err)
	}
	f, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("the lock is still held after a failed start: %v", err)
	}
	_ = f.Close()
}

// The real hand-over: a child given the locked file as fd 3 keeps the lock
// after the launcher closes its copy, and releases it when it exits. A plain
// `sleep` stands in for the daemon; no daemon is started.
func TestLaunchLock_ChildKeepsItAfterTheLauncherCloses(t *testing.T) {
	withTempRuntime(t)
	lock, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "30")
	child.ExtraFiles = []*os.File{lock}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	_ = lock.Close()
	if f, err := acquireDaemonLock(); !errors.Is(err, errDaemonAlreadyRunning) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("after the launcher closed its copy the lock was free (%v); the child must hold it", err)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	f, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("the lock was not released when the child exited: %v", err)
	}
	_ = f.Close()
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
		adopted, err := adoptDaemonLock(os.NewFile(uintptr(dup), "dup"), daemonLockPath())
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
		if _, err := adoptDaemonLock(other, daemonLockPath()); err == nil {
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
		if _, err := adoptDaemonLock(f, daemonLockPath()); err == nil {
			t.Fatal("adopted a description that does not hold the lock")
		}
	})
}

func TestDaemonLifetimeLock_RefusesABadInheritedFD(t *testing.T) {
	withTempRuntime(t)
	for _, v := range []string{"", "4", "x"} {
		t.Setenv(daemonLockFDEnv, v)
		if f, err := daemonLifetimeLock(); err == nil {
			_ = f.Close()
			t.Fatalf("%s=%q: started without the launch lock", daemonLockFDEnv, v)
		}
		if _, ok := os.LookupEnv(daemonLockFDEnv); ok {
			t.Fatalf("%s must be unset before the daemon starts children", daemonLockFDEnv)
		}
	}
}

// A `plumb daemon` started directly (a service manager, a container CMD) has
// no inherited lock and acquires its own, as before.
func TestDaemonLifetimeLock_WithoutALauncherAcquiresItsOwn(t *testing.T) {
	withTempRuntime(t)
	t.Setenv(daemonLockFDEnv, "")
	os.Unsetenv(daemonLockFDEnv) //nolint:usetesting // must be absent, not empty; t.Setenv above restores it
	lock, err := daemonLifetimeLock()
	if err != nil {
		t.Fatalf("a directly started daemon could not take its lock: %v", err)
	}
	defer lock.Close()
	if f, err := acquireDaemonLock(); !errors.Is(err, errDaemonAlreadyRunning) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("the lock is not held: %v", err)
	}
}

func TestDaemonStartWait(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
	}{
		{"", 30 * time.Second},
		{"45s", 45 * time.Second},
		{"2m", 2 * time.Minute},
		{"0", 30 * time.Second},
		{"-5s", 30 * time.Second},
		{"soon", 30 * time.Second},
		{"1h", 5 * time.Minute},
		{"45", 45 * time.Second},
	} {
		t.Setenv("PLUMB_DAEMON_START_TIMEOUT", c.env)
		if got := daemonStartWait(); got != c.want {
			t.Errorf("PLUMB_DAEMON_START_TIMEOUT=%q: got %s, want %s", c.env, got, c.want)
		}
	}
}

func fcntlGetFD(fd int) (int, error) {
	r, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}
