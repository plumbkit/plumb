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

// PrintLogo renders the industrial "piping" logo with an optional subtitle.
// Subtitle is left-aligned and styled with tui.ItemStyle.
func PrintLogo(subtitle string) {
	tui.RebuildStyles()
	logoStyle := lipgloss.NewStyle().Foreground(tui.ActiveTheme.Accent)
	fmt.Println(logoStyle.Render(logoText))
	if subtitle != "" {
		fmt.Println(tui.ItemStyle.Render(subtitle))
	}
	fmt.Println()
}
