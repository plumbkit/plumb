package cli

import (
	"fmt"
	"io"
	"os"

	"charm.land/lipgloss/v2"

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
