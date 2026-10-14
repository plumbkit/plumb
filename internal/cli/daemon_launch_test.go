package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// testLauncher is a launcher on the real launch locks (in a temp runtime dir)
// whose dial succeeds once ready reports true. Its start counts, and keeps a
// duplicate of each lock open until the test ends: a stand-in for a live child
// that inherited them.
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
		acquire: acquireLaunchLocks,
		start: func(locks launchLocks) error {
			starts.Add(1)
			for _, f := range locks.files() {
				child, err := syscall.Dup(int(f.Fd()))
				if err != nil {
					return err
				}
				t.Cleanup(func() { _ = syscall.Close(child) })
			}
			return nil
		},
		wait: wait,
		poll: 10 * time.Millisecond,
	}
}

// freeLaunchLocks asserts that no launch lock is held any more, naming the one
// that is: a launcher that returns must not sit on a lock it did not hand over.
// The legacy lifetime locks are part of that set (PLAN-530): they are taken
// before a launch and must be released with everything else when it does not
// happen.
func freeLaunchLocks(t *testing.T) {
	t.Helper()
	for name, acquire := range map[string]func() (*os.File, error){
		"account authority": acquireAuthority,
		"lifetime":          acquireDaemonLock,
	} {
		f, err := acquire()
		if err != nil {
			t.Fatalf("the %s lock is still held: %v", name, err)
		}
		_ = f.Close()
	}
	for _, loc := range legacyLocations() {
		held, err := acquireLegacyLock(loc.Lock)
		if err != nil {
			t.Fatalf("the legacy lifetime lock %s is still held: %v", loc.Lock, err)
		}
		_ = held.File.Close()
	}
}

func TestDialOrLaunch_ARunningDaemonIsDialledWithoutTheLock(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	ready.Store(true)
	l := testLauncher(t, &ready, &starts, time.Second)
	acquired := false
	l.acquire = func() (launchLocks, error) { acquired = true; return acquireLaunchLocks() }
	conn, launched, err := l.dialOrLaunch(context.Background())
	if err != nil || conn == nil || launched {
		t.Fatalf("got conn=%v launched=%v err=%v, want a dial and no launch", conn, launched, err)
	}
	if acquired || starts.Load() != 0 {
		t.Fatalf("a daemon that answers must not be launched beside: acquired=%v starts=%d", acquired, starts.Load())
	}
}

// The race #649 fixes: a daemon is starting (it holds the locks) but does not
// accept yet. The launcher must never start a second one; it waits and, at its
// deadline, fails naming the silent holder.
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

// A held account authority means a daemon exists, wherever its environment put
// it: the launcher spawns nothing and waits for that daemon instead.
func TestDialOrLaunch_HeldAuthoritySpawnsNothing(t *testing.T) {
	withTempRuntime(t)
	holder, err := acquireAuthority()
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
		t.Fatalf("started %d daemon(s) while another held the account authority", starts.Load())
	}
}

// The authority is taken first, so a lifetime lock that turns out to be held
// must give it back: a launcher that lost the race may not leave the account
// authority held by a process that is about to return.
func TestDialOrLaunch_HeldLifetimeLockReleasesTheAuthority(t *testing.T) {
	withTempRuntime(t)
	holder, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, 200*time.Millisecond)
	_, launched, err := l.dialOrLaunch(context.Background())
	if !errors.Is(err, errDaemonHeldButSilent) || launched || starts.Load() != 0 {
		t.Fatalf("got launched=%v err=%v starts=%d, want a wait and no launch", launched, err, starts.Load())
	}
	authority, err := acquireAuthority()
	if err != nil {
		t.Fatalf("the account authority is still held after the lifetime lock was found held: %v", err)
	}
	_ = authority.Close()
}

// The same rule as the launcher test above, at the acquire level and with no
// launcher in the way: the authority is taken first, so a lifetime lock that is
// already held must give it back — a launcher that lost the race may not leave
// the account authority held by a process that is about to return.
func TestAcquireLaunchLocks_ReleasesTheAuthorityWhenTheLifetimeLockIsHeld(t *testing.T) {
	withTempRuntime(t)
	holder, err := acquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := acquireLaunchLocks(); !errors.Is(err, errDaemonAlreadyRunning) {
		t.Fatalf("acquireLaunchLocks = %v, want errDaemonAlreadyRunning", err)
	}
	authority, err := acquireAuthority()
	if err != nil {
		t.Fatalf("the account authority is still held after the lifetime lock was found held: %v", err)
	}
	_ = authority.Close()
}

// The one order, launcher side: a held authority is reported without the runtime
// lifetime lock ever being opened. Pinned behaviourally — the lifetime lock path
// is made unopenable, so a launcher that took it first would report that open
// error instead of the held authority.
func TestAcquireLaunchLocks_TakesTheAuthorityBeforeTheLifetimeLock(t *testing.T) {
	withTempRuntime(t)
	holder, err := acquireAuthority()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	brokeLifetimeLockPath(t)

	if _, err := acquireLaunchLocks(); !errors.Is(err, errDaemonAlreadyRunning) {
		t.Fatalf("acquireLaunchLocks = %v, want errDaemonAlreadyRunning", err)
	}
}

// A client whose own environment resolves elsewhere must reach the one daemon:
// while the authority is held, the launcher dials the socket that holder
// recorded, before its bounded redial gives up.
func TestDialOrLaunch_HeldAuthorityDialsTheRecordedSocket(t *testing.T) {
	withTempRuntime(t)
	holder, err := acquireAuthority()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, 5*time.Second)
	recorded, other := net.Pipe()
	t.Cleanup(func() { _ = recorded.Close(); _ = other.Close() })
	var dialled atomic.Int32
	l.dialHeld = func() (net.Conn, bool) { dialled.Add(1); return recorded, true }

	conn, launched, err := l.dialOrLaunch(context.Background())
	if err != nil || conn == nil || launched {
		t.Fatalf("got conn=%v launched=%v err=%v, want the recorded socket and no launch", conn, launched, err)
	}
	if dialled.Load() == 0 {
		t.Error("the held-authority path never dialled the socket the holder recorded")
	}
	if conn != recorded {
		t.Error("the connection returned is not the one the recorded socket gave")
	}
	if starts.Load() != 0 {
		t.Fatalf("started %d daemon(s) beside the authority's holder", starts.Load())
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

// `plumb restart`: the stopped daemon still holds the locks while it exits.
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
	l.start = func(locks launchLocks) error {
		// The launcher holds both locks while it starts the child.
		if other, err := acquireAuthority(); !errors.Is(err, errDaemonAlreadyRunning) {
			if other != nil {
				_ = other.Close()
			}
			t.Errorf("start ran without the account authority: %v", err)
		}
		if other, err := acquireDaemonLock(); !errors.Is(err, errDaemonAlreadyRunning) {
			if other != nil {
				_ = other.Close()
			}
			t.Errorf("start ran without the lifetime lock: %v", err)
		}
		ready.Store(true)
		return inner(locks)
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
	l.start = func(launchLocks) error { starts.Add(1); return nil } // no child keeps the locks
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
	freeLaunchLocks(t)
}

func TestDialOrLaunch_ReturnsALockError(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, time.Second)
	boom := errors.New("opening daemon lock: permission denied")
	l.acquire = func() (launchLocks, error) { return launchLocks{}, boom }
	if _, _, err := l.dialOrLaunch(context.Background()); !errors.Is(err, boom) || starts.Load() != 0 {
		t.Fatalf("got err=%v starts=%d, want the lock error and no start", err, starts.Load())
	}
}

// A launcher that holds neither lock must not spawn: starting a daemon beside
// one that already exists is the defect the locks exist to prevent.
func TestDialOrLaunch_NoLocksHeldStartsNothing(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, time.Second)
	l.acquire = func() (launchLocks, error) { return launchLocks{}, nil }
	_, launched, err := l.dialOrLaunch(context.Background())
	if !errors.Is(err, errNoLaunchLocks) || launched || starts.Load() != 0 {
		t.Fatalf("got launched=%v err=%v starts=%d, want errNoLaunchLocks and no start", launched, err, starts.Load())
	}
}

// The production launcher takes both locks, hands them to the child, and knows
// the attach path; a test launcher replaces those, nothing else does.
func TestNewDaemonLauncher_WiresBothLocksAndTheAttachPath(t *testing.T) {
	withTempRuntime(t)
	l := newDaemonLauncher("/tmp/plumb/plumb.sock")
	if l.dial == nil {
		t.Fatal("the launcher must be able to dial a daemon socket")
	}
	for _, c := range []struct {
		name string
		got  any
		want any
	}{
		{"acquire", l.acquire, acquireLaunchLocks},
		{"start", l.start, startDaemonProcess},
		{"dialHeld", l.dialHeld, dialHeldAuthority},
	} {
		if c.got == nil {
			t.Fatalf("the launcher's %s is not wired", c.name)
		}
		if reflect.ValueOf(c.got).Pointer() != reflect.ValueOf(c.want).Pointer() {
			t.Errorf("the launcher's %s is not the production one", c.name)
		}
	}
}

// The launcher must not keep its copies of the locks: a long-lived serve would
// otherwise hold the authority itself and block its own later reconnect.
func TestDialOrLaunch_ClosesItsCopyOfTheLock(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, time.Second)
	var passed launchLocks
	inner := l.start
	l.start = func(locks launchLocks) error { passed = locks; ready.Store(true); return inner(locks) }
	if _, _, err := l.dialOrLaunch(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, f := range passed.files() {
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("the launcher's copy of a lock is still open (Stat: %v)", err)
		}
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

func TestDialOrLaunch_AFailedStartReleasesTheLocks(t *testing.T) {
	withTempRuntime(t)
	var ready atomic.Bool
	var starts atomic.Int32
	l := testLauncher(t, &ready, &starts, time.Second)
	l.start = func(launchLocks) error { return errors.New("exec failed") }
	if _, _, err := l.dialOrLaunch(context.Background()); err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Fatalf("got %v, want the start error", err)
	}
	freeLaunchLocks(t)
}

// The real hand-over: a child given the locked files as fd 3 and fd 4 keeps the
// locks after the launcher closes its copies, and releases them when it exits. A
// plain `sleep` stands in for the daemon; no daemon is started.
func TestLaunchLocks_ChildKeepsThemAfterTheLauncherCloses(t *testing.T) {
	withTempRuntime(t)
	locks, err := acquireLaunchLocks()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "30")
	child.ExtraFiles = locks.files() // fd 3 and fd 4 in the child
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	locks.closeAll()
	for name, acquire := range map[string]func() (*os.File, error){
		"account authority": acquireAuthority,
		"lifetime":          acquireDaemonLock,
	} {
		if f, err := acquire(); !errors.Is(err, errDaemonAlreadyRunning) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("after the launcher closed its copies the %s lock was free (%v); the child must hold it", name, err)
		}
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	freeLaunchLocks(t)
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
