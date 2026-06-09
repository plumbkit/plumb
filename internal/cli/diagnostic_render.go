package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/reflow/wordwrap"

	"github.com/plumbkit/plumb/internal/tui"
)

type cliDiagnostic struct {
	Kind        string
	Title       string
	Body        string
	Suggestions []string
}

const diagnosticGutter = "    "

func printCLIDiagnostic(w io.Writer, d cliDiagnostic) {
	out := os.Stderr
	if f, ok := w.(*os.File); ok {
		out = f
	}
	fmt.Fprint(w, renderCLIDiagnostic(d, terminalWidth(out)))
}

func renderCLIDiagnostic(d cliDiagnostic, width int) string {
	tui.RebuildStyles()

	if d.Kind == "" {
		d.Kind = "error"
	}
	if d.Title == "" {
		d.Title = d.Kind
	}

	contentWidth := max(width-len(diagnosticGutter)-2, 24)

	title := diagnosticTitle(d)
	lines := []string{title}
	lines = append(lines, diagnosticBorderedLines(strings.TrimRight(d.Body, "\n"), contentWidth)...)
	if len(d.Suggestions) > 0 {
		lines = append(lines, diagnosticBorderedLines("", contentWidth)...)
		lines = append(lines, diagnosticBorderedLines("Try:", contentWidth)...)
		for _, suggestion := range d.Suggestions {
			lines = append(lines, diagnosticBorderedLines("  "+suggestion, contentWidth)...)
		}
	}

	return strings.Join(lines, "\n") + "\n"
}

func diagnosticTitle(d cliDiagnostic) string {
	switch strings.ToLower(d.Kind) {
	case "info":
		titleStyle := tui.ItemStyle.Bold(true)
		return diagnosticBadge(" i ", titleStyle) + " " + titleStyle.Render(d.Title)
	default:
		titleStyle := tui.WarnStyle.Bold(true)
		return diagnosticBadge(" ✗ ", titleStyle) + " " + titleStyle.Render(d.Title)
	}
}

func diagnosticBadge(label string, titleStyle lipgloss.Style) string {
	return lipgloss.NewStyle().
		Bold(true).
		Background(titleStyle.GetForeground()).
		Foreground(lipgloss.Color("0")).
		Render(label)
}

func diagnosticBorderedLines(text string, width int) []string {
	border := diagnosticGutter + tui.SepStyle.Render("┊")
	if text == "" {
		return []string{border}
	}

	wrapped := wordwrap.String(text, width)
	rawLines := strings.Split(wrapped, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		if after, ok := strings.CutPrefix(line, "  "); ok {
			lines = append(lines, border+"  "+tui.HintStyle.Render(after))
			continue
		}
		lines = append(lines, border+" "+tui.MutedStyle.Render(line))
	}
	return lines
}

func terminalWidth(f *os.File) int {
	width, _, err := term.GetSize(f.Fd())
	if err != nil || width <= 0 {
		return 80
	}
	return width
}
