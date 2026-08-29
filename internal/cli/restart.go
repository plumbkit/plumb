package cli

import (
	"context"
	"fmt"
	"net"
	"slices"
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

func runRestart(_ *cobra.Command, _ []string) error {
	PrintLogo()
	pids := findAllDaemonPIDs()
	if len(pids) == 0 {
		fmt.Println("Daemon is not running.")
		return respawnDaemon(nil)
	}

	prompted := false
	if !restartFlagForce {
		ok, shown, err := confirmDaemonActionWithActiveSessions(restartActionPrompt)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("\nRestart cancelled.")
			return nil
		}
		prompted = shown
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
	return respawnDaemon(pids)
}

// respawnDaemon brings a daemon back up after a stop. It always announces the
// starting phase before checking whether one is already there — a resilient
// client may win the race and respawn it first (the initial dial succeeds),
// in which case we spawn nothing ourselves, but the command is still the one
// telling the operator a restart is underway, so the announcement does not
// depend on which process ends up calling startDaemonProcess. Absent that
// race we spawn one ourselves under the shared spawn lock — the same
// dial-or-spawn dance `plumb serve` uses, minus the serve-specific logging and
// the stale-version warning (irrelevant right after a restart).
func respawnDaemon(stopped []int) error {
	fmt.Println("Starting...")
	socketPath := daemonSocketPath()
	if dialDaemonOnce(socketPath) {
		printDaemonRestarted(stopped)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	spawn, err := acquireSpawnLock(ctx)
	if err != nil {
		return fmt.Errorf("waiting to spawn daemon: %w", err)
	}
	defer spawn.Close()

	// Re-check under the lock — a concurrent serve may have spawned it.
	if dialDaemonOnce(socketPath) {
		printDaemonRestarted(stopped)
		return nil
	}
	if err := startDaemonProcess(); err != nil {
		return fmt.Errorf("starting daemon: %w", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if dialDaemonOnce(socketPath) {
			printDaemonRestarted(stopped)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return daemonStartTimeoutError("come back up", socketPath)
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
