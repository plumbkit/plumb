package cli

import (
	"fmt"
	"io"
	"net"

	"github.com/spf13/cobra"
)

func init() {
	debugCmd.AddCommand(debugMemCmd, debugHeapCmd)
}

var debugCmd = &cobra.Command{
	Use:   "debug",
	Short: "Inspect the running daemon's internals (memory, heap profiles)",
	Long: `Low-level daemon introspection for diagnosing resource use.

  plumb debug mem    — print a live runtime memory snapshot
  plumb debug heap   — write a heap pprof profile and print its path

Both talk to the running daemon over its control socket; start it with
"plumb serve" if it is not already up.`,
}

var debugMemCmd = &cobra.Command{
	Use:   "mem",
	Short: "Print the running daemon's live memory stats",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		resp, err := dialDaemonCtrlFull("mem-stats")
		if err != nil {
			return err
		}
		fmt.Print(resp)
		return nil
	},
}

var debugHeapCmd = &cobra.Command{
	Use:   "heap",
	Short: "Write a heap pprof profile from the running daemon and print its path",
	Long: `Force a GC in the daemon and write a heap profile to the cache directory.

Inspect it with:  go tool pprof <printed-path>`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		resp, err := dialDaemonCtrlFull("heap-profile")
		if err != nil {
			return err
		}
		fmt.Print(resp)
		return nil
	},
}

// dialDaemonCtrlFull dials the daemon control socket, sends a single-line
// command, and returns the full (possibly multi-line) response. It differs from
// dialDaemonCtrl, which reads only the first response line — mem-stats replies
// span several lines.
func dialDaemonCtrlFull(command string) (string, error) {
	conn, err := net.Dial("unix", daemonCtrlSocketPath())
	if err != nil {
		return "", fmt.Errorf("daemon control socket unavailable — is plumb daemon running?\n  start it with: plumb serve\n  (%w)", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", command); err != nil {
		return "", fmt.Errorf("sending command: %w", err)
	}
	out, err := io.ReadAll(conn)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	return string(out), nil
}
