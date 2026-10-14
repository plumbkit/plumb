package cli

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

var restartFlagForce bool

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the background daemon",
	Long: `Stop the running daemon and immediately start a fresh one.

Follows the same rules as 'plumb stop': it shows active sessions and asks for
confirmation before proceeding (--force skips the prompt). With the resilient
'plumb serve' proxy (0.8.0+), connected clients reconnect to the new daemon
automatically, so a restart is transparent to active conversations — it does
not take longer than a stop, and no client has to re-establish its session.`,
	RunE: runRestart,
}

func init() {
	restartCmd.Flags().BoolVar(&restartFlagForce, "force", false, "restart without asking for confirmation")
}

// restartConfirm and restartRespawn are the two steps of a restart that a test
// cannot run for real — one blocks on a human, the other starts a daemon — so the
// suite substitutes them to pin WHEN the restart's deadline starts.
var (
	restartConfirm = confirmDaemonActionWithActiveSessions
	restartRespawn = respawnDaemon
)

func runRestart(_ *cobra.Command, _ []string) error {
	PrintLogo()
	pids := restartDaemonPIDs()
	if len(pids) == 0 {
		fmt.Println("Daemon is not running.")
		return restartRespawn(nil, time.Now().Add(daemonStartWait()))
	}

	prompted := false
	if !restartFlagForce {
		ok, shown, err := restartConfirm(restartActionPrompt)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("\nRestart cancelled.")
			return nil
		}
		prompted = shown
		// The prompt waits on a human, and a pid named before it may since have
		// exited and been handed to somebody else. Re-identify now, so what is
		// signalled is what is a daemon of ours at the moment of the signal.
		if pids = restartDaemonPIDs(); len(pids) == 0 {
			fmt.Println("Daemon is not running.")
			return restartRespawn(nil, time.Now().Add(daemonStartWait()))
		}
	}

	if len(pids) > 1 {
		fmt.Printf("Found %d daemon process(es) — stopping all.\n", len(pids))
	}
	for i, pid := range pids {
		if err := stopByPID(pid, prompted && i == 0); err != nil {
			return err
		}
		forceKillIfAlive(pid) // a daemon that ignored SIGTERM must still go, so respawn is real
	}
	// ONE deadline for the exit check and the successor's start, and it is the
	// launcher deadline: how long a plumb launch waits for a daemon before giving
	// up. It starts HERE, after the confirmation and the stop — both bounded on
	// their own (or by a human) — so a slow answer to the prompt cannot spend it and
	// leave the account with no daemon at all (PLAN-552 MINOR-4).
	deadline := time.Now().Add(daemonStartWait())
	// Invariant 4, and the reason this is not folded into respawnDaemon: the
	// successor starts only after the old daemon's EXIT is confirmed. A pid that
	// is still there after SIGTERM and SIGKILL is a daemon that may still be
	// serving, and starting beside it is the two-daemon window whatever the
	// authority says. Fail closed, start nothing.
	if err := waitForDaemonExit(pids, deadline); err != nil {
		return err
	}
	return restartRespawn(pids, deadline)
}

// daemonIdentityCheck reports whether a pid is a `plumb daemon` process: the
// check every pid a restart read from a FILE must pass before it is signalled.
//
// It is a variable so the suite can pin the policy. A real `plumb daemon` cannot
// be started outside a container (PLAN-518 §2: exactly one daemon runs on the
// owner's host, and it is not a test's to start), and the stand-ins the tests use
// are ordinary processes — so a test that needs the POSITIVE direction, a claimed
// pid that IS named, substitutes this, while the negative direction is pinned
// against the real function itself.
var daemonIdentityCheck = isPlumbDaemonProcess

// restartDaemonPIDs names the daemons a restart must stop, by IDENTITY — never
// by a process scan (PLAN-530, and the reason `plumb stop`'s third strategy is
// scoped so carefully: an unscoped `pgrep -f "plumb daemon"` sweep once SIGTERMed
// an operator's live daemon three times mid-session while it served eleven other
// connections).
//
// Four identities, all of them statements by or about a daemon that exists here:
//
//   - the pid the current HOLDER OF THE AUTHORITY published through the held fd,
//     which is the one daemon every client reaches;
//   - the pid file of this process's runtime directory, and of every known legacy
//     runtime location — an older binary's daemon keeps its own;
//   - the process that owns the MCP or control socket in any of those
//     directories, which covers a daemon that never wrote a pid file.
//
// A process that only LOOKS like a plumb daemon is not stopped by a restart,
// because nothing about it says it is one of ours. That is the fail-closed
// direction: a stray daemon surviving a restart is visible and recoverable, and
// killing somebody else's is neither.
func restartDaemonPIDs() []int {
	seen := make(map[int]bool)
	var pids []int
	add := func(pid int) {
		if pid > 0 && !seen[pid] && processAlive(pid) {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}
	// A pid read from a FILE is a CLAIM, not an observation. The file outlives the
	// process that wrote it — nothing unlinks the authority lock, and a pid file
	// survives a crash — while the kernel hands a freed number to somebody else.
	// Every claim is verified against the process it names before restart will
	// signal it. The harm is asymmetric: a daemon a restart failed to stop is
	// visible and recoverable; killing an unrelated process is neither.
	addClaim := func(pid int) {
		if pid > 0 && !seen[pid] && daemonIdentityCheck(pid) {
			add(pid)
		}
	}

	// 1. The authority's holder, as it published itself — and ONLY while somebody
	//    actually holds the authority. The record is written through the held fd
	//    and the file is never unlinked, so after a crash, a `plumb stop` or a
	//    reboot it still names the pid of a daemon that is gone while the lock
	//    itself is free; and a free lock means the pid it records may since have
	//    belonged to somebody else. An unreadable record is not a failure here:
	//    the identities below still name the daemon, and the bounded lock
	//    acquisition in respawnDaemon refuses to start a successor while the
	//    authority is held by anybody at all.
	if path, err := authorityPath(); err == nil {
		if m, ok := readAuthority(path); ok && authorityHeld() {
			addClaim(m.PID)
		}
	}

	// 2. Every runtime directory this process could belong to, current first: the
	//    pid file each daemon writes, and the sockets each one binds.
	for _, loc := range append([]legacyLocation{currentLocation()}, legacyLocations()...) {
		addClaim(readPIDFileAt(loc.PID))
		add(findPIDViaSocket(loc.Socket))
		add(findPIDViaSocket(loc.Ctrl))
	}
	return pids
}

// waitForDaemonExit blocks until every pid this restart stopped is gone, or the
// deadline passes — and then it is an ERROR, not a warning: the caller is about
// to start a second daemon, and a daemon that is still alive is exactly the one
// it must not start beside.
//
// processAlive is kill(pid, 0), so a process that has exited but not been reaped
// still reads as alive. That is the right answer here: the daemon holds its
// sockets and its locks until it is truly gone.
func waitForDaemonExit(pids []int, deadline time.Time) error {
	for {
		var alive []int
		for _, pid := range pids {
			if processAlive(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("refusing to start a daemon: %s did not exit within %s, so a successor would be a second daemon; "+
				"stop it (SIGKILL if it is wedged) and run `plumb restart` again",
				pidList(alive), daemonStartWait())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// pidList renders pids for a message ("PID 12" / "PIDs 12, 14").
func pidList(pids []int) string {
	if len(pids) == 1 {
		return fmt.Sprintf("PID %d", pids[0])
	}
	parts := make([]string, 0, len(pids))
	for _, pid := range pids {
		parts = append(parts, strconv.Itoa(pid))
	}
	return "PIDs " + strings.Join(parts, ", ")
}

// respawnDaemon brings a daemon back up after a stop, and it is the half of
// `plumb restart` that PLAN-530 had to change most.
//
// Before the successor is started, this command takes EVERY launch lock — the
// account authority, the runtime lifetime lock, and the lifetime lock of every
// known legacy runtime location — and hands them to the child on ExtraFiles. The
// acquisition is also the confirmation invariant 4 asks for: the authority's
// release is not inferred from a signal having been sent, it is established by
// TAKING the lock, and it is bounded by the same deadline as the exit check. If
// either cannot be confirmed, nothing is started.
//
// The reason for all of it is the handover gap. Every lock a daemon holds is
// released the instant it exits, and an old proxy whose daemon has gone
// reconnects on its own schedule: it dials, fails, takes the legacy lifetime lock
// and starts a legacy successor. A successor started by this command could not
// win that race, because there is a window between the old daemon's exit and the
// child's first instruction. Holding the legacy locks across that window closes
// it — and the child inherits them, so it stays closed for the new daemon's whole
// life.
func respawnDaemon(stopped []int, deadline time.Time) error {
	fmt.Println("Starting...")
	socketPath := daemonSocketPath()
	return respawnDaemonWith(newDaemonLauncher(socketPath), socketPath, stopped, deadline)
}

// respawnDaemonWith is respawnDaemon with the launcher injected, so the
// lock-confirmation rules and the handover can be driven — and proved — without
// starting a real daemon.
func respawnDaemonWith(l daemonLauncher, socketPath string, stopped []int, deadline time.Time) error {
	// A resilient client may have won the race and brought the successor up
	// already: its proxy redials the moment the old daemon's socket closes, and it
	// starts one under the same locks this command would take. The AUTHORITY is
	// what tells that apart from a leftover socket — a socket that answers with no
	// live authority behind it is not a daemon this command should adopt, and the
	// acquisition below then replaces it.
	if dialDaemonOnce(socketPath) && authorityRecorded() {
		printDaemonRestarted(stopped)
		return nil
	}

	locks, err := acquireLaunchLocksUntil(deadline)
	if err != nil {
		return fmt.Errorf("refusing to start a daemon: %w", err)
	}
	defer locks.closeAll()

	// The held locks go to the launcher exactly once, as if this process had just
	// acquired them; from then on the launcher sees them held — by the daemon it
	// started — and only dials.
	l.acquire = takeHeldLocks(&locks)

	lockCtx, cancelLock := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancelLock()
	spawn, err := acquireSpawnLock(lockCtx)
	if err != nil {
		return fmt.Errorf("waiting to spawn daemon: %w", err)
	}
	release := sync.OnceFunc(func() { _ = spawn.Close() })
	defer release()
	l.decided = release

	// The launcher also waits out a stopped daemon still releasing its lock, and
	// never starts one beside a daemon that holds it. Its own context, so time
	// spent on the stop and the spawn lock does not eat into the start-up wait.
	ctx, cancel := context.WithTimeout(context.Background(), l.wait+2*time.Second)
	defer cancel()
	conn, _, err := l.dialOrLaunch(ctx)
	if err != nil {
		return launchError("come back up", socketPath, l.wait, err)
	}
	_ = conn.Close()
	printDaemonRestarted(stopped)
	return nil
}

// acquireLaunchLocksUntil takes every launch lock, retrying until the deadline,
// so the caller both CONFIRMS that the previous daemon released them and holds
// them for the successor.
//
// A held LEGACY lock is not retried: it means an older launcher's daemon owns a
// runtime location, which waiting cannot change, and the remedy is the
// instruction that error carries. Everything else waits, because a daemon that
// was signalled a moment ago is still closing its sockets and its locks.
func acquireLaunchLocksUntil(deadline time.Time) (launchLocks, error) {
	var last error
	for {
		locks, err := acquireLaunchLocks()
		if err == nil {
			return locks, nil
		}
		last = err
		if isLegacyLockHeld(err) {
			return launchLocks{}, err
		}
		if !time.Now().Before(deadline) {
			return launchLocks{}, fmt.Errorf("the launch lock was still held %s after the daemon was stopped, "+
				"so it may still be serving and nothing was started: %w", daemonStartWait(), last)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// takeHeldLocks hands held to the launcher exactly once, then reports the locks
// held — which is what a launcher sees when somebody else owns them.
func takeHeldLocks(held *launchLocks) func() (launchLocks, error) {
	return func() (launchLocks, error) {
		if held.empty() {
			return launchLocks{}, errDaemonAlreadyRunning
		}
		out := *held
		*held = launchLocks{}
		return out, nil
	}
}

// printDaemonRestarted reports the daemon is back up, including the PID of the
// process now serving. stopped lists the PIDs this restart just terminated, and
// they are excluded deliberately: see waitForDaemonPID.
func printDaemonRestarted(stopped []int) {
	if pid := waitForDaemonPID(stopped); pid > 0 {
		fmt.Printf("Daemon restarted (PID %d).\n", pid)
		return
	}
	fmt.Println("Daemon restarted.")
}

// waitForDaemonPID resolves the PID of the daemon that is serving now, never one
// of the stopped PIDs this restart just killed.
//
// Two races make the naive read wrong. The daemon writes its PID file just after
// opening the listening socket (runDaemon in daemon.go), so a dial that succeeds
// in the same instant can find the file not yet written — hence the grace
// window. And the outgoing daemon does not erase the file on the way out, so
// within that window a read can return the number of the corpse we just
// SIGTERM'd, printing "Daemon restarted (PID <the one we killed>)". Excluding
// stopped is what makes the printed PID mean what it says.
//
// If the file is still stale when the window closes, fall back to whoever owns
// the socket: the dial already proved someone is serving, and that process is
// the authority on who it is — better than dropping the PID from the line.
func waitForDaemonPID(stopped []int) int {
	deadline := time.Now().Add(daemonPIDGrace)
	for {
		if pid := readDaemonPID(); pid > 0 && !slices.Contains(stopped, pid) {
			return pid
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if pid := findPIDViaSocket(daemonSocketPath()); pid > 0 && !slices.Contains(stopped, pid) {
		return pid
	}
	return 0
}

// daemonPIDGrace is how long waitForDaemonPID waits for the fresh daemon to
// publish its PID file. A var, not a const, so the tests can shrink it: the
// stale-file path is only reachable by letting the window close.
var daemonPIDGrace = 2 * time.Second

// dialDaemonOnce reports whether the daemon socket accepts a connection.
func dialDaemonOnce(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
