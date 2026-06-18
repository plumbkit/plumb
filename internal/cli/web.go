package cli

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var (
	webNoOpen bool
	webPort   int
)

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Open the plumb web UI (opt-in, loopback-only)",
	Long: `Start the daemon's web UI and open it in your browser.

The web UI is an opt-in, loopback-only dashboard with full parity to the
terminal UI: Dashboard, Sessions, Memory, Logs, and the scope-aware Settings
editor with theme picker. It binds 127.0.0.1 only, behind a per-start token —
nothing is exposed beyond this machine.

  plumb web              — start the web UI and open the browser
  plumb web --no-open    — start it and just print the URL
  plumb web --port 9000  — bind a specific loopback port for this launch

The port defaults to [web].port in the config (8870). The daemon is started
automatically if it is not already running.`,
	Args: cobra.NoArgs,
	RunE: runWeb,
}

func init() {
	webCmd.Flags().BoolVar(&webNoOpen, "no-open", false, "print the URL instead of opening a browser")
	webCmd.Flags().IntVar(&webPort, "port", 0, "loopback port to bind (overrides config for this launch)")
}

func runWeb(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// Ensure the daemon is running: dial-or-spawn the MCP socket, then release it
	// — we only need the process up so its control socket answers web-start.
	conn, err := connectOrStartDaemon(ctx, daemonSocketPath())
	if err != nil {
		return fmt.Errorf("ensuring daemon is running: %w", err)
	}
	_ = conn.Close()

	command := "web-start"
	if webPort != 0 {
		command += " " + strconv.Itoa(webPort)
	}
	resp, err := dialDaemonCtrl(command)
	if err != nil {
		return err
	}
	if msg, ok := strings.CutPrefix(resp, "error:"); ok {
		return fmt.Errorf("starting web UI: %s", strings.TrimSpace(msg))
	}

	url := strings.TrimSpace(resp)
	fmt.Printf("plumb web UI: %s\n", url)
	if webNoOpen {
		return nil
	}
	if err := openBrowser(url); err != nil {
		fmt.Printf("(could not open a browser automatically: %v)\n", err)
	}
	return nil
}

// openBrowser opens url in the user's default browser. It is best-effort: a
// failure is reported to the caller, which falls back to printing the URL.
func openBrowser(url string) error {
	var name string
	args := make([]string, 0, 2)
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	args = append(args, url)
	return exec.Command(name, args...).Start() //nolint:gosec // G204: name is a fixed per-OS opener, url is our own loopback URL
}
