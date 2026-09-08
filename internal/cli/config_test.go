package cli

import (
	"os"
	"path/filepath"
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

func TestConfigShowHelpDescribesMergedCommandProjection(t *testing.T) {
	if got, want := configShowCmd.Short, "Show resolved configuration and provenance"; got != want {
		t.Fatalf("config show short help = %q, want %q", got, want)
	}
	for _, want := range []string{
		"merged for this command",
		"does not report a running daemon's current state",
		"plumb debug lsp for running servers",
	} {
		if !strings.Contains(configShowCmd.Long, want) {
			t.Errorf("config show help must say %q:\n%s", want, configShowCmd.Long)
		}
	}
}

func TestConfigShowReportsLSPCommandProjection(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	oldWorkspace, oldAdapters := configShowWorkspace, configShowAdapters
	configShowWorkspace, configShowAdapters = workspace, false
	defer func() {
		configShowWorkspace, configShowAdapters = oldWorkspace, oldAdapters
	}()

	out := stripANSI(captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	}))
	for _, want := range []string{
		"eligible (this command)",
		"merged config + PATH",
		"LSP eligibility is derived from this command's merged config and PATH; use plumb debug lsp for running servers.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config show output must contain %q:\n%s", want, out)
		}
	}
}
