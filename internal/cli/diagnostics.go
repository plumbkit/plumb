package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tui"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runDiagOnWorkspace(ctx, cli, cwd)
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

func runDiagOnWorkspace(ctx context.Context, cli *mcpCliClient, cwd string) error {
	// Resolve the actual workspace root via find_files (returns paths under
	// the project root).
	_, _ = cli.CallTool("list_files", map[string]any{"path": cwd, "max_results": 1})

	printDiagHeader(cwd)

	globs, langLabel := detectDiagnosticsLang(cwd)

	// Walk every source file in the workspace via find_files. A language may map
	// to several globs (e.g. *.ts and *.tsx) because find_files has no brace
	// expansion; scan each and concatenate. The extensions are disjoint, so the
	// combined list needs no de-duplication.
	var srcFiles []string
	for _, glob := range globs {
		listOut, err := cli.CallTool("find_files", map[string]any{
			"pattern":     glob,
			"path":        cwd,
			"max_results": 500,
		})
		if err != nil {
			return fmt.Errorf("listing files: %w", err)
		}
		srcFiles = append(srcFiles, parseFileList(listOut, cwd)...)
	}
	if len(srcFiles) == 0 {
		fmt.Printf("No %s files found in %s\n", langLabel, cwd)
		return nil
	}

	totalClean, totalIssues, totalUntracked, perFile := scanWorkspaceDiags(ctx, cli, srcFiles)
	if ctx.Err() != nil {
		fmt.Println(tui.WarnStyle.Render("! scan stopped early — partial results below"))
		fmt.Println()
	}

	scanned := totalClean + totalIssues + totalUntracked
	summary := fmt.Sprintf("Scanned %s: %d clean · %d with issues · %d not tracked",
		pluralisedFiles(scanned, langLabel), totalClean, totalIssues, totalUntracked)
	fmt.Println(tui.ItemStyle.Render(summary))

	if totalIssues > 0 {
		fmt.Println()
		for _, p := range perFile {
			fmt.Println(styleDiagnostics(p))
		}
	}

	if totalIssues == 0 && totalUntracked == 0 {
		fmt.Println()
		fmt.Println(tui.OkStyle.Render("✓ All files clean."))
	} else if totalIssues > 0 {
		if totalUntracked > 0 {
			fmt.Println()
		}
	} else if totalUntracked > 0 {
		fmt.Println()
	}

	if totalUntracked > 0 {
		noteStr := fmt.Sprintf("%s 'not tracked' files have not been opened by the language server.\n%s",
			tui.ItemStyle.Bold(true).Render("Note:"),
			tui.MutedStyle.Render("↳ Run a tool that touches each (or open in your editor) to force analysis."),
		)

		fmt.Println(render.ContextBox(noteStr, tui.SepStyle))
	}
	return nil
}

// detectDiagnosticsLang returns the file globs and display label for the primary
// source language detected in ws by checking well-known root markers. The go.mod
// / go.work markers lead the list so an explicit Go project always wins over a
// weak co-located marker (a package.json from frontend tooling, a static
// index.html). Falls back to Go when no recognised marker is found.
//
// A language can map to several globs because find_files matches with
// filepath.Match, which has no brace expansion — so .ts and .tsx are separate
// patterns the caller scans in turn.
func detectDiagnosticsLang(ws string) (globs []string, label string) {
	markers := []struct {
		file  string
		globs []string
		label string
	}{
		{"go.mod", []string{"*.go"}, "Go"},
		{"go.work", []string{"*.go"}, "Go"},
		{"Package.swift", []string{"*.swift"}, "Swift"},
		{"Cargo.toml", []string{"*.rs"}, "Rust"},
		{"pyproject.toml", []string{"*.py"}, "Python"},
		{"setup.py", []string{"*.py"}, "Python"},
		{"tsconfig.json", []string{"*.ts", "*.tsx"}, "TypeScript"},
		{"package.json", []string{"*.ts", "*.tsx", "*.js", "*.jsx", "*.mjs", "*.cjs"}, "TypeScript/JavaScript"},
		{"index.html", []string{"*.html", "*.htm"}, "HTML"},
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(ws, m.file)); err == nil {
			return m.globs, m.label
		}
	}
	return []string{"*.go"}, "Go"
}

func parseFileList(output, cwd string) []string {
	var files []string
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Found") || strings.HasPrefix(line, "No ") || strings.HasPrefix(line, "(") {
			continue
		}
		// find_files prints relative paths from cwd.
		abs := line
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, line)
		}
		files = append(files, abs)
	}
	return files
}

// startScanProgress starts an animating spinner for the file scan.
// Returns a setProgress func (call with 1-based index per file) and a stop
// func that clears the line. Both are no-ops when stdout is not a terminal.
// When ctx is cancelled the spinner switches to a "Stopping…" message while
// the in-flight RPC for the current file completes.
func startScanProgress(ctx context.Context, total int) (setProgress func(i int), stop func()) {
	if !stdoutIsTerminal() {
		return func(int) {}, func() {}
	}
	var current atomic.Int64
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		spin := spinner.MiniDot
		ticker := time.NewTicker(spin.FPS)
		defer ticker.Stop()
		frame := 0
		ctxDone := ctx.Done()
		stopping := false
		for {
			select {
			case <-ctxDone:
				stopping = true
				ctxDone = nil
				fmt.Println() // leave the last scanning line intact; stopping goes below
			case <-done:
				return
			case <-ticker.C:
				if stopping {
					fmt.Printf("\r%s Stopping...",
						tui.HintStyle.Render(spin.Frames[frame]))
				} else {
					i := current.Load()
					fmt.Printf("\r%s Scanning %d/%d files...",
						tui.HintStyle.Render(spin.Frames[frame]), i, total)
				}
				frame = (frame + 1) % len(spin.Frames)
			}
		}
	}()
	return func(i int) { current.Store(int64(i)) },
		func() { close(done); <-stopped; fmt.Printf("\r\033[K") }
}

func scanWorkspaceDiags(ctx context.Context, cli *mcpCliClient, goFiles []string) (clean, issues, untracked int, perFile []string) {
	setProgress, stopProgress := startScanProgress(ctx, len(goFiles))
	defer stopProgress()
	for i, f := range goFiles {
		select {
		case <-ctx.Done():
			return clean, issues, untracked, perFile
		default:
		}
		setProgress(i + 1)
		uri := "file://" + f
		out, err := cli.CallTool("diagnostics", map[string]any{"uri": uri})
		if err != nil {
			continue
		}
		switch {
		case strings.Contains(out, "not yet tracked"):
			untracked++
		case strings.Contains(out, "clean"):
			clean++
		case strings.Contains(out, "issue"):
			issues++
			perFile = append(perFile, out)
		}
	}
	return clean, issues, untracked, perFile
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

		// If it's a source file path
		if isSourceFilePath(trimmed) {
			contracted := render.ContractPath(trimmed)
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

// isSourceFilePath reports whether a diagnostics output line looks like a
// source file path (used to decide whether to render it as a bold heading).
func isSourceFilePath(s string) bool {
	exts := []string{".go", ".swift", ".rs", ".py", ".ts", ".js", ".kt", ".java", ".html", ".htm"}
	for _, e := range exts {
		if strings.HasSuffix(s, e) {
			return true
		}
	}
	return false
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

	ctxStr := render.ContractPath(workspace)
	if match != nil {
		ctxStr += fmt.Sprintf("\nsession %s", match.ID)
	}

	fmt.Println(render.ContextBox(tui.MutedStyle.Render(ctxStr), tui.SepStyle))
	fmt.Println()
}

func pluralisedFiles(n int, lang string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s file", lang)
	}
	return fmt.Sprintf("%d %s files", n, lang)
}

// removed unused encoding/json import — keep for future use.
var _ = json.RawMessage(nil)
