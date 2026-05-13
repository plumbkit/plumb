package cli

import (
	"fmt"

	"charm.land/lipgloss/v2"
	"github.com/golimpio/plumb/internal/tui"
)

const logoText = `╭──╮ ╷          ╷
├──╯ │ ╷  ╭─┬─╮ ├──╮
╵    ╵ ││ ╵ ╵ ╵ ╰──╯
─────╮ ╰╯ ╭─────────`

// PrintLogo renders the industrial "piping" logo.
func PrintLogo() {
	tui.RebuildStyles()
	logoStyle := lipgloss.NewStyle().Foreground(tui.ActiveTheme.Accent)
	fmt.Println(logoStyle.Render(logoText))
	fmt.Println()
}
