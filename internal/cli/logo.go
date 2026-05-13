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

// ContextBorder is a left-only dotted border for workspace context blocks.
var ContextBorder = lipgloss.Border{
	Left: "┊",
}

// DottedBorder is a fully dotted border for CLI tables.
var DottedBorder = lipgloss.Border{
	Top:         "╌",
	Bottom:      "╌",
	Left:        "┊",
	Right:       "┊",
	TopLeft:     "╭",
	TopRight:    "╮",
	BottomLeft:  "╰",
	BottomRight: "╯",
	Middle:      "┼",
	MiddleTop:   "┬",
	MiddleBottom: "┴",
	MiddleLeft:  "├",
	MiddleRight: "┤",
}
