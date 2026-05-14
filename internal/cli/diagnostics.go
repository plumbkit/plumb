package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/tui"
)

var diagnosticsCmd = &cobra.Command{
	Use:     "diagnostics [file]",
	Aliases: []string{"diag", "diags"},
	Short:   "Print LSP diagnostics for the workspace (debugging tool)",
	Long: `Connect to the running plumb daemon and run gopls diagnostics, printing
the result to stdout.

Behaviour:
  - With a [file] argument: per-file diagnostics for that file only.
  - Without arguments: walks every Go file in the current directory tree,
    explicitly opening each via the MCP diagnostics tool so gopls analyses
    them. This avoids the "still indexing" trap — gopls only emits push
    diagnostics for files it has seen via didOpen.

Output is prefixed with a banner showing which daemon session produced it,
so you can correlate with 'plumb sessions'.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDiagnostics,
}

func runDiagnostics(_ *cobra.Command, args []string) error {
	PrintLogo()

	socket := daemonSocketPath()
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return fmt.Errorf("daemon not running.\n\nStart Claude Desktop or run a tool first to bring it up.\nSocket: %s", socket)
	}
	cli := newMcpCliClient(conn)
	defer cli.Close()

	if err := cli.Initialize("plumb-cli-diagnostics", Version); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	// File argument: per-file diagnostics. No argument: walk the workspace
	// and aggregate file-level diagnostics for every Go file. The aggregate
	// path is intentional — gopls only emits diagnostics for files it has
	// seen via didOpen, so we have to explicitly request each one.
	if len(args) > 0 {
		abs, err := filepath.Abs(args[0])
		if err != nil {
			return fmt.Errorf("resolving %q: %w", args[0], err)
		}
		if _, err := os.Stat(abs); err != nil {
			return fmt.Errorf("file not found: %s", abs)
		}
		return runDiagOnFile(cli, abs)
	}
	return runDiagOnWorkspace(cli, cwd)
}

func runDiagOnFile(cli *mcpCliClient, abs string) error {
	uri := "file://" + abs
	// Warm-up the workspace via a path-bearing call so the daemon attaches gopls.
	_, _ = cli.CallTool("list_files", map[string]any{"path": filepath.Dir(abs), "max_results": 1})
	printDiagHeader(filepath.Dir(abs))
	out, err := cli.CallTool("diagnostics", map[string]any{"uri": uri})
	if err != nil {
		return fmt.Errorf("diagnostics: %w", err)
	}
	fmt.Println(styleDiagnostics(out))
	return nil
}

func runDiagOnWorkspace(cli *mcpCliClient, cwd string) error {
	// Resolve the actual workspace root via find_files (returns paths under
	// the project root).
	_, _ = cli.CallTool("list_files", map[string]any{"path": cwd, "max_results": 1})

	printDiagHeader(cwd)

	// Walk every Go file in the workspace via find_files.
	listOut, err := cli.CallTool("find_files", map[string]any{
		"pattern":     "*.go",
		"path":        cwd,
		"max_results": 500,
	})
	if err != nil {
		return fmt.Errorf("listing files: %w", err)
	}

	var goFiles []string
	for _, line := range strings.Split(listOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Found") || strings.HasPrefix(line, "No ") || strings.HasPrefix(line, "(") {
			continue
		}
		// find_files prints relative paths from cwd.
		abs := line
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, line)
		}
		goFiles = append(goFiles, abs)
	}

	if len(goFiles) == 0 {
		fmt.Println("No Go files found in", cwd)
		return nil
	}

	totalIssues := 0
	totalUntracked := 0
	totalClean := 0
	var perFile []string

	for i, f := range goFiles {
		fmt.Printf("\r%s Scanning %d/%d files...", tui.HintStyle.Render("⟳"), i+1, len(goFiles))
		uri := "file://" + f
		out, err := cli.CallTool("diagnostics", map[string]any{"uri": uri})
		if err != nil {
			continue
		}
		switch {
		case strings.Contains(out, "not yet tracked"):
			totalUntracked++
		case strings.Contains(out, "clean"):
			totalClean++
		case strings.Contains(out, "issue"):
			totalIssues++
			perFile = append(perFile, out)
		}
	}
	fmt.Printf("\r\033[K") // clear the progress line

	summary := fmt.Sprintf("Scanned %d Go file(s): %d clean · %d with issues · %d not tracked",
		len(goFiles), totalClean, totalIssues, totalUntracked)
	fmt.Println(tui.ItemStyle.Render(summary))
	fmt.Println()

	for _, p := range perFile {
		fmt.Println(styleDiagnostics(p))
	}
	if totalIssues == 0 && totalUntracked == 0 {
		fmt.Println(tui.OkStyle.Render("✓ All files clean."))
	} else if totalIssues == 0 {
		// Only print a blank line if there were no issues printed above
		// (if there were issues, the last issue printed handles its own spacing).
	} else {
		fmt.Println()
	}

	if totalUntracked > 0 {
		noteStr := fmt.Sprintf("%s 'not tracked' files have not been opened by gopls.\n%s",
			tui.ItemStyle.Bold(true).Render("Note:"),
			tui.MutedStyle.Render("↳ Run a tool that touches each (or open in your editor) to force analysis."),
		)
		
		noteBox := lipgloss.NewStyle().
			Border(ContextBorder, false, false, false, true).
			BorderForeground(tui.SepStyle.GetForeground()).
			PaddingLeft(1).
			Render(noteStr)
			
		fmt.Println(noteBox)
	}
	return nil
}

func styleDiagnostics(raw string) string {
	lines := strings.Split(raw, "\n")
	var out []string
	for _, line := range lines {
		if strings.Contains(line, "issue(s) across") {
			// Skip the total summary line from the tool, we print our own or group by file.
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		
		// If it's a file path
		if strings.HasSuffix(trimmed, ".go") {
			contracted := contractSessionPath(trimmed)
			out = append(out, "", tui.HintStyle.Bold(true).Render(contracted))
			continue
		}

		// If it's a diagnostic line: "  ERROR  99:13  message"
		if strings.HasPrefix(line, "  ") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 3 {
				sev := parts[0]
				pos := parts[1]
				msg := strings.Join(parts[2:], " ")
				
				var style lipgloss.Style
				switch sev {
				case "ERROR":
					style = tui.WarnStyle
				case "WARN":
					style = tui.SelectedStyle // Using amber/yellow
				default:
					style = tui.MutedStyle
				}
				
				out = append(out, fmt.Sprintf("  %s %s  %s", 
					style.Width(8).Render(sev), 
					tui.MutedStyle.Render(pos), 
					msg))
				continue
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// header prints the session/workspace banner so the caller knows which
// daemon session produced the output.
func printDiagHeader(workspace string) {
	tui.RebuildStyles()
	sessions, _ := session.List()
	var match *session.Info
	for i := range sessions {
		if sessions[i].ClientName == "plumb-cli-diagnostics" {
			match = &sessions[i]
		}
	}
	
	ctxStr := contractSessionPath(workspace)
	if match != nil {
		ctxStr += fmt.Sprintf("\nsession %s", match.ID)
	}

	ctxBox := lipgloss.NewStyle().
		Border(ContextBorder, false, false, false, true).
		BorderForeground(tui.SepStyle.GetForeground()).
		PaddingLeft(1).
		Render(tui.MutedStyle.Render(ctxStr))

	fmt.Println(ctxBox)
	fmt.Println()
}

// removed unused encoding/json import — keep for future use.
var _ = json.RawMessage(nil)
