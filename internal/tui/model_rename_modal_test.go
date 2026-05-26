package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestRenameModalBoxWidthsConsistent guards the right-border regression: every
// rendered line of the box must share the same visible width, otherwise the
// closing │ / ╮ / ╯ column ragged-edges (the original fmt.Sprintf width math
// was off by one on the input row).
func TestRenameModalBoxWidthsConsistent(t *testing.T) {
	RebuildStyles()
	cases := map[string]renameSessionModal{
		"empty":     newRenameSessionModal("wild-viper"),
		"typing":    {currentName: "wild-viper", input: "new-name"},
		"invalid":   {currentName: "wild-viper", input: "Bad Name!", validationErr: "name may only contain letters, digits and hyphens"},
		"longinput": {currentName: "wild-viper", input: strings.Repeat("x", 80)},
	}
	for name, modal := range cases {
		t.Run(name, func(t *testing.T) {
			lines := strings.Split(modal.box(), "\n")
			if len(lines) < 3 {
				t.Fatalf("box has too few lines: %d", len(lines))
			}
			want := lipgloss.Width(lines[0])
			for i, l := range lines {
				if got := lipgloss.Width(l); got != want {
					t.Errorf("line %d width = %d, want %d (%q)", i, got, want, l)
				}
			}
		})
	}
}

// TestRenameModalCentred guards the centring regression: the box must not bake
// its own padding (the old View() left-padded then spliceOverlay re-centred,
// shoving the box to the right). spliceOverlay alone should centre it, so the
// background margins on either side of the box differ by at most one column.
func TestRenameModalCentred(t *testing.T) {
	RebuildStyles()
	const w, h = 120, 40
	bgLines := make([]string, h)
	for i := range bgLines {
		bgLines[i] = strings.Repeat(".", w)
	}

	modal := newRenameSessionModal("wild-viper")
	out := modal.renderModal(strings.Join(bgLines, "\n"), w, h)

	for _, line := range strings.Split(out, "\n") {
		plain := ansi.Strip(line)
		if !strings.Contains(plain, "Rename Session") {
			continue
		}
		if got := lipgloss.Width(plain); got != w {
			t.Fatalf("spliced title line width = %d, want %d", got, w)
		}
		// Box-drawing chars are multi-byte; work in runes (each is one column).
		runes := []rune(plain)
		left, rightIdx := -1, -1
		for i, c := range runes {
			switch c {
			case '╭':
				left = i
			case '╮':
				rightIdx = i
			}
		}
		if left < 0 || rightIdx < 0 {
			t.Fatalf("could not locate box borders in %q", plain)
		}
		right := len(runes) - rightIdx - 1
		if diff := left - right; diff > 1 || diff < -1 {
			t.Errorf("box not centred: left margin %d, right margin %d", left, right)
		}
		return
	}
	t.Fatal("did not find the modal title line in the spliced output")
}
