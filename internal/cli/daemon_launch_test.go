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
// whose dial succeeds once ready reports true, and whose start only counts.
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
		start: func(*os.File) error {
			starts.Add(1)
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
