package cli

import (
	"fmt"
	"io"
	"os"

	"charm.land/lipgloss/v2"
	"github.com/golimpio/plumb/internal/tui"
)

const logoText = `╭─╮ ╷        ╷
┣━┛ ┃ ╷  ┏┳┓ ┣━┓
╵   ╵ ┃┃ ╵╵╵ ╰─╯
────╮ ╰╯ ╭──────`

var logoPrinted bool

// PrintLogo renders the industrial "piping" logo.
func PrintLogo() {
	printLogo(os.Stdout)
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

// ContextBorder is a left-only dotted border for workspace context blocks.
var ContextBorder = lipgloss.Border{
	Left: "┊",
}

// DottedBorder is a fully dotted border for CLI tables.
var DottedBorder = lipgloss.Border{
	Top:          "╌",
	Bottom:       "╌",
	Left:         "┊",
	Right:        "┊",
	TopLeft:      "╭",
	TopRight:     "╮",
	BottomLeft:   "╰",
	BottomRight:  "╯",
	Middle:       "┼",
	MiddleTop:    "┬",
	MiddleBottom: "┴",
	MiddleLeft:   "├",
	MiddleRight:  "┤",
}
