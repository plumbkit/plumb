package cli

import (
	"strings"
	"testing"
)

func TestRenderConfigShowTableBorderShape(t *testing.T) {
	tbl := configShowTableBase().
		Headers("Name", "Value").
		Row("alpha", "1").
		Row("beta", "2")

	plain := stripANSI(renderConfigShowTable(tbl))
	lines := strings.Split(plain, "\n")
	if len(lines) != 7 {
		t.Fatalf("expected 7 rendered table lines, got %d:\n%s", len(lines), plain)
	}

	if !strings.HasPrefix(lines[0], "╭") || !strings.HasSuffix(lines[0], "╮") || strings.Contains(lines[0], "╌") {
		t.Fatalf("top border should be continuous:\n%s", lines[0])
	}
	if !strings.HasPrefix(lines[6], "╰") || !strings.HasSuffix(lines[6], "╯") || strings.Contains(lines[6], "╌") {
		t.Fatalf("bottom border should be continuous:\n%s", lines[6])
	}
	if !strings.Contains(lines[2], "─") || strings.Contains(lines[2], "╌") {
		t.Fatalf("header separator should be continuous:\n%s", lines[2])
	}
	if !strings.Contains(lines[4], "╌") {
		t.Fatalf("row separator should stay dotted:\n%s", lines[4])
	}
	if !strings.HasPrefix(lines[3], "│") || !strings.HasSuffix(lines[3], "│") || strings.Contains(lines[3], "┊") {
		t.Fatalf("data row should use continuous vertical separators:\n%s", lines[3])
	}
	if got := strings.Count(lines[3], "│"); got < 3 {
		t.Fatalf("data row should include continuous column separators, got %d:\n%s", got, lines[3])
	}
}
