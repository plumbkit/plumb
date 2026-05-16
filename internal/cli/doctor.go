package cli

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/stats"
	"github.com/golimpio/plumb/internal/tui"
)

var doctorWorkspace string

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check plumb's health and configuration",
	Long: `Run a series of health checks and report the status of plumb's
daemon, language servers, MCP client registrations, and configuration.

Use --workspace to include per-project checks (stats DB, project config).`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().StringVar(&doctorWorkspace, "workspace", "",
		"Workspace directory to include in project-scoped checks (defaults to current dir)")
}

type checkResult struct {
	name   string
	ok     bool
	detail string
	fix    string // one-line hint printed when ok=false
}

func runDoctor(_ *cobra.Command, _ []string) error {
	ws := doctorWorkspace
	if ws == "" {
		if cwd, err := os.Getwd(); err == nil {
			ws = cwd
		}
	}

	tui.RebuildStyles()
	PrintLogo()

	var checks []checkResult

	checks = append(checks, checkDaemon()...)
	checks = append(checks, checkLSPs(ws)...)
	checks = append(checks, checkMCPClients()...)
	checks = append(checks, checkConfigs(ws)...)
	checks = append(checks, checkStatsDB(ws)...)

	printChecks(checks)

	failures := 0
	for _, c := range checks {
		if !c.ok {
			failures++
		}
	}
	fmt.Println()
	if failures == 0 {
		fmt.Println(tui.OkStyle.Render("All checks passed."))
	} else {
		fmt.Printf("%s  %d check(s) need attention — see fix hints above.\n",
			tui.WarnStyle.Render("✗"), failures)
	}
	return nil
}

// checkDaemon verifies the daemon is reachable and its version matches.
func checkDaemon() []checkResult {
	socketPath := daemonSocketPath()
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		return []checkResult{{
			name:   "daemon reachable",
			ok:     false,
			detail: fmt.Sprintf("cannot dial %s", contractSessionPath(socketPath)),
			fix:    "run `plumb serve` in a terminal or let an MCP client start it automatically",
		}}
	}
	conn.Close()

	results := []checkResult{{
		name:   "daemon reachable",
		ok:     true,
		detail: contractSessionPath(socketPath),
	}}

	// Version check.
	data, err := os.ReadFile(daemonVersionPath())
	if err != nil {
		results = append(results, checkResult{
			name:   "daemon version",
			ok:     false,
			detail: "version file missing — old daemon?",
			fix:    "run `plumb stop` then reconnect to restart with the current binary",
		})
		return results
	}
	running := string(bytes.TrimSpace(data))
	if running == Version || running == "" {
		results = append(results, checkResult{
			name:   "daemon version",
			ok:     true,
			detail: running,
		})
	} else {
		results = append(results, checkResult{
			name:   "daemon version",
			ok:     false,
			detail: fmt.Sprintf("running %s, binary is %s", running, Version),
			fix:    "run `plumb stop` then reconnect to reload the current binary",
		})
	}
	return results
}

// checkLSPs verifies that language server binaries are on PATH.
func checkLSPs(ws string) []checkResult {
	cfg, err := config.Load()
	if err != nil {
		return []checkResult{{
			name:   "lsp config",
			ok:     false,
			detail: err.Error(),
			fix:    "fix global config at " + contractConfigPath(config.GlobalConfigPath()),
		}}
	}
	if ws != "" {
		if merged, err := config.LoadProject(cfg, ws); err == nil {
			cfg = merged
		}
	}

	var results []checkResult
	for lang, lsp := range cfg.LSP {
		if !lsp.Enabled || lsp.Command == "" {
			continue
		}
		path, err := exec.LookPath(lsp.Command)
		if err != nil {
			results = append(results, checkResult{
				name:   "lsp " + lang,
				ok:     false,
				detail: lsp.Command + " not found on PATH",
				fix:    "install " + lsp.Command + " and ensure it is on your PATH",
			})
			continue
		}
		// Try --version to confirm it's executable.
		out, err := exec.Command(path, "--version").Output()
		version := strings.TrimSpace(string(out))
		if err != nil || version == "" {
			results = append(results, checkResult{
				name:   "lsp " + lang,
				ok:     true,
				detail: path + " (version unknown)",
			})
		} else {
			results = append(results, checkResult{
				name:   "lsp " + lang,
				ok:     true,
				detail: path + "  " + version,
			})
		}
	}
	return results
}

// checkMCPClients checks whether plumb is registered with each known MCP client.
func checkMCPClients() []checkResult {
	type client struct {
		name   string
		pathFn func() (string, error)
	}
	clients := []client{
		{"Claude Desktop", claudeDesktopConfigPath},
		{"Claude Code", claudeCodeConfigPath},
		{"Gemini CLI", GeminiConfigPath},
		{"Codex", CodexConfigPath},
	}

	var results []checkResult
	for _, c := range clients {
		path, err := c.pathFn()
		if err != nil {
			results = append(results, checkResult{
				name:   "mcp " + strings.ToLower(c.name),
				ok:     false,
				detail: "cannot locate config: " + err.Error(),
			})
			continue
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			results = append(results, checkResult{
				name:   "mcp " + strings.ToLower(c.name),
				ok:     false,
				detail: c.name + " config not found",
				fix:    fmt.Sprintf("install %s, then run `plumb setup %s`", c.name, setupCmdName(c.name)),
			})
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			results = append(results, checkResult{
				name:   "mcp " + strings.ToLower(c.name),
				ok:     false,
				detail: "cannot read config: " + err.Error(),
			})
			continue
		}
		if strings.Contains(string(data), "plumb") {
			results = append(results, checkResult{
				name:   "mcp " + strings.ToLower(c.name),
				ok:     true,
				detail: contractConfigPath(path),
			})
		} else {
			results = append(results, checkResult{
				name:   "mcp " + strings.ToLower(c.name),
				ok:     false,
				detail: c.name + " config exists but plumb is not registered",
				fix:    fmt.Sprintf("run `plumb setup %s`", setupCmdName(c.name)),
			})
		}
	}
	return results
}

// checkConfigs verifies global and project config files are parseable.
func checkConfigs(ws string) []checkResult {
	var results []checkResult

	globalPath := config.GlobalConfigPath()
	if _, err := os.Stat(globalPath); os.IsNotExist(err) {
		results = append(results, checkResult{
			name:   "global config",
			ok:     true,
			detail: "not present (using defaults)",
		})
	} else if _, err := config.Load(); err != nil {
		results = append(results, checkResult{
			name:   "global config",
			ok:     false,
			detail: err.Error(),
			fix:    "fix TOML syntax in " + contractConfigPath(globalPath),
		})
	} else {
		results = append(results, checkResult{
			name:   "global config",
			ok:     true,
			detail: contractConfigPath(globalPath),
		})
	}

	if ws == "" {
		return results
	}
	projectPath := config.ProjectConfigPath(ws)
	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		results = append(results, checkResult{
			name:   "project config",
			ok:     true,
			detail: "not present (inheriting global)",
		})
	} else {
		base, _ := config.Load()
		if _, err := config.LoadProject(base, ws); err != nil {
			results = append(results, checkResult{
				name:   "project config",
				ok:     false,
				detail: err.Error(),
				fix:    "fix TOML syntax in " + contractConfigPath(projectPath),
			})
		} else {
			results = append(results, checkResult{
				name:   "project config",
				ok:     true,
				detail: contractConfigPath(projectPath),
			})
		}
	}
	return results
}

// checkStatsDB verifies the per-workspace stats DB is readable.
func checkStatsDB(ws string) []checkResult {
	if ws == "" {
		return nil
	}
	dbPath := stats.DBPathFor(ws)
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return []checkResult{{
			name:   "stats db",
			ok:     true,
			detail: "not present yet (created on first tool call)",
		}}
	}
	db, err := stats.OpenReadOnly(dbPath)
	if err != nil {
		return []checkResult{{
			name:   "stats db",
			ok:     false,
			detail: err.Error(),
			fix:    "the DB may be corrupt — remove " + contractConfigPath(dbPath) + " to reset",
		}}
	}
	total := db.TotalCalls(stats.Filter{})
	db.Close()
	return []checkResult{{
		name:   "stats db",
		ok:     true,
		detail: fmt.Sprintf("%s  (%d calls recorded)", contractConfigPath(dbPath), total),
	}}
}

func printChecks(checks []checkResult) {
	t := table.New().
		Border(DottedBorder).
		BorderRow(false).
		BorderColumn(false).
		BorderLeft(false).
		BorderRight(false).
		BorderTop(true).
		BorderBottom(false).
		BorderStyle(tui.SepStyle).
		Headers("Check", "Status", "Detail").
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().PaddingRight(2)
			if row == table.HeaderRow {
				return s.Inherit(tui.HintStyle)
			}
			return s
		})

	for _, c := range checks {
		status := tui.OkStyle.Render("✓  ok")
		if !c.ok {
			status = tui.WarnStyle.Render("✗  fail")
		}
		detail := c.detail
		if !c.ok && c.fix != "" {
			detail += "\n" + tui.MutedStyle.Render("fix: "+c.fix)
		}
		t.Row(c.name, status, detail)
	}
	fmt.Println(t.Render())
}

func setupCmdName(clientName string) string {
	switch clientName {
	case "Claude Desktop":
		return "claude-desktop"
	case "Claude Code":
		return "claude-code"
	case "Gemini CLI":
		return "gemini"
	case "Codex":
		return "codex"
	}
	return strings.ToLower(strings.ReplaceAll(clientName, " ", "-"))
}
