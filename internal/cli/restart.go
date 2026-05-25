package cli

import (
	"context"
	"fmt"
	"net"
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
		fmt.Println("Daemon is not running — starting a fresh one.")
		return respawnDaemon()
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
	}
	return respawnDaemon()
}

// respawnDaemon brings a daemon back up after a stop. A resilient client may
// already have respawned it the instant the old one exited (the dial succeeds);
// otherwise we spawn one ourselves under the shared spawn lock — the same
// dial-or-spawn dance `plumb serve` uses, minus the serve-specific logging and
// the stale-version warning (irrelevant right after a restart).
func respawnDaemon() error {
	socketPath := daemonSocketPath()
	if dialDaemonOnce(socketPath) {
		fmt.Println("Daemon restarted.")
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
		fmt.Println("Daemon restarted.")
		return nil
	}
	if err := startDaemonProcess(); err != nil {
		return fmt.Errorf("starting daemon: %w", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if dialDaemonOnce(socketPath) {
			fmt.Println("Daemon restarted.")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not come back up within 10 seconds (socket: %s)", socketPath)
}

// dialDaemonOnce reports whether the daemon socket accepts a connection.
func dialDaemonOnce(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
