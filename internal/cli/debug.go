package cli

import (
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/render"
)

func init() {
	debugCmd.AddCommand(debugMemCmd, debugHeapCmd, debugStacksCmd)
}

var debugCmd = &cobra.Command{
	Use:   "debug",
	Short: "Inspect the running daemon's internals (memory, heap profiles, goroutine stacks)",
	Long: `Low-level daemon introspection for diagnosing resource use.

  plumb debug mem    — print a live runtime memory snapshot
  plumb debug heap   — write a heap pprof profile and print its path
  plumb debug stacks — dump every goroutine's stack and print the file path

These talk to the running daemon over its control socket; start it with
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
		pairs := parseMemPairs(resp)
		if len(pairs) == 0 {
			fmt.Print(resp) // error reply or unexpected shape — show it raw
			return nil
		}
		for _, row := range render.LeaderRows(pairs) {
			fmt.Println(row)
		}
		return nil
	},
}

// parseMemPairs turns the daemon's tab-separated mem-stats reply into ordered
// label/value pairs. Returns nil if any line is not a label\tvalue pair (e.g.
// an "error: …" reply), so the caller can fall back to printing it raw.
func parseMemPairs(resp string) [][2]string {
	var pairs [][2]string
	for _, line := range strings.Split(strings.TrimRight(resp, "\n"), "\n") {
		if line == "" {
			continue
		}
		label, value, ok := strings.Cut(line, "\t")
		if !ok {
			return nil
		}
		pairs = append(pairs, [2]string{label, value})
	}
	return pairs
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

var debugStacksCmd = &cobra.Command{
	Use:   "stacks",
	Short: "Dump every goroutine's stack from the running daemon and print the file path",
	Long: `Write a full goroutine stack dump from the daemon to the cache directory.

This is the non-destructive equivalent of SIGQUIT: every goroutine's stack,
showing what each is blocked on. Capture it *during* a hang to see the wedge —
then grep the file (e.g. for "sync.Mutex", "conn.Write", or a lock name) to find
the blocked goroutines.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		resp, err := dialDaemonCtrlFull("goroutine-stacks")
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
