package cli

import (
	"fmt"
	"io"
	"os"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/tui"
)

const logoText = `╭─╮ ╷        ╷
┣━┛ ┃ ╷  ┏┳┓ ┣━┓
╵   ╵ ┃┃ ╵╵╵ ╰─╯
────╮ ╰╯ ╭──────`

// annoSkipLogo marks a command that must NOT print the logo banner before it
// runs — the stdio-protocol commands (serve, daemon) and the bare TUI launch,
// where a banner on stdout would corrupt the MCP wire or the alt-screen.
const annoSkipLogo = "skipLogo"

// suppressLogo reports whether the banner must be withheld for this invocation.
//
// Two reasons. The static one is the annoSkipLogo annotation (serve, daemon, the
// bare TUI). The dynamic one is a `--json` flag set on this run: the annotation
// is per-command, but machine-readable output is per-invocation, and
// `plumb doctor --json` printed the banner to stdout ahead of the JSON — so every
// consumer got a parse error on line 1. Keyed on the flag rather than on the
// command name, so any future command gaining --json is covered without
// remembering this rule.
func suppressLogo(cmd *cobra.Command) bool {
	if cmd.Annotations[annoSkipLogo] == "true" {
		return true
	}
	if f := cmd.Flags().Lookup("json"); f != nil && f.Value.String() == "true" {
		return true
	}
	return false
}

var logoPrinted bool

// PrintLogo renders the industrial "piping" logo once per process; repeat
// calls are no-ops so the PersistentPreRun banner and a command's own call
// never double-print.
func PrintLogo() {
	printLogoIfNeeded(os.Stdout)
}

func printLogo(w io.Writer) {
	logoPrinted = true
	tui.RebuildStyles()
	logoStyle := lipgloss.NewStyle().Foreground(tui.ActiveTheme.Accent)
	fmt.Fprintln(w, logoStyle.Render(logoText))
	fmt.Fprintln(w)
}

func printLogoIfNeeded(w io.Writer) {
	if logoPrinted {
		return
	}
	printLogo(w)
}
