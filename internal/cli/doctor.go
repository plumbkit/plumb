package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/render"
	"github.com/golimpio/plumb/internal/stats"
	"github.com/golimpio/plumb/internal/tui"
)

var doctorWorkspace string

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check plumb's health and configuration",
	Long: `Run health checks grouped by topic and report the status of plumb's
daemon, language servers, MCP client registrations, and configuration.

Use --workspace to include project-scoped checks (stats rows, project config).`,
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

	sections := []struct {
		title string
		run   func() []checkResult
	}{
		{"Daemon", checkDaemon},
		{"Language Servers", func() []checkResult { return checkLSPs(ws) }},
		{"MCP Clients", checkMCPClients},
		{"Configuration", func() []checkResult { return checkConfigs(ws) }},
		{"Data", func() []checkResult { return checkStatsDB(ws) }},
	}

	failures := 0
	for _, s := range sections {
		checks := runSection(s.title, s.run)
		for _, c := range checks {
			if !c.ok {
				failures++
			}
		}
	}

	if failures == 0 {
		fmt.Println(tui.OkStyle.Render("All checks passed."))
	} else {
		fmt.Printf("%s  %d check(s) need attention — see hints above.\n",
			tui.WarnStyle.Render("✗"), failures)
	}
	return nil
}

func runSection(title string, run func() []checkResult) []checkResult {
	fmt.Println(tui.HintStyle.Render("● " + title))
	fmt.Println()
	stopWorking := startWorkingIndicator()
	checks := run()
	stopWorking()
	printChecks(checks)
	fmt.Println()
	return checks
}

// printChecks prints checks aligned by name column.
func printChecks(checks []checkResult) {
	nameW := 0
	for _, c := range checks {
		if len(c.name) > nameW {
			nameW = len(c.name)
		}
	}

	for _, c := range checks {
		marker := tui.OkStyle.Render("✓")
		name := fmt.Sprintf("%-*s", nameW, c.name)
		if !c.ok {
			marker = tui.WarnStyle.Render("✗")
			name = tui.WarnStyle.Render(name)
		}
		detailLines := strings.Split(c.detail, "\n")
		detail := detailLines[0]
		if !c.ok {
			detail = tui.WarnStyle.Render(detail)
		}
		fmt.Printf("  %s  %s  %s\n", marker, name, detail)
		indent := strings.Repeat(" ", 7+nameW)
		for _, line := range detailLines[1:] {
			if !c.ok {
				line = tui.WarnStyle.Render(line)
			}
			fmt.Printf("%s%s\n", indent, line)
		}
		if !c.ok && c.fix != "" {
			fmt.Printf("%s%s\n", indent, tui.WarnStyle.Render("→ "+c.fix))
		}
	}
}

func startWorkingIndicator() func() {
	if !stdoutIsTerminal() {
		return func() {}
	}
	done := make(chan struct{})
	printed := make(chan bool, 1)
	go func() {
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			spin := spinner.MiniDot
			ticker := time.NewTicker(spin.FPS)
			defer ticker.Stop()
			printed <- true
			frame := 0
			for {
				fmt.Fprintf(os.Stdout, "\r  %s working...", tui.HintStyle.Render(spin.Frames[frame]))
				frame = (frame + 1) % len(spin.Frames)
				select {
				case <-ticker.C:
				case <-done:
					return
				}
			}
		case <-done:
			printed <- false
		}
	}()
	return func() {
		close(done)
		if <-printed {
			fmt.Fprint(os.Stdout, "\r\033[2K")
		}
	}
}

func stdoutIsTerminal() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// checkDaemon verifies the daemon is reachable and its version matches.
func checkDaemon() []checkResult {
	socketPath := daemonSocketPath()
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		return []checkResult{{
			name:   "socket",
			ok:     false,
			detail: fmt.Sprintf("cannot dial %s", render.ContractPath(socketPath)),
			fix:    "run `plumb serve` or let an MCP client start it automatically",
		}}
	}
	conn.Close()

	results := []checkResult{{
		name:   "socket",
		ok:     true,
		detail: render.ContractPath(socketPath),
	}}

	data, err := os.ReadFile(daemonVersionPath())
	if err != nil {
		results = append(results, checkResult{
			name:   "version",
			ok:     false,
			detail: "version file missing — daemon may be stale",
			fix:    "run `plumb stop` then reconnect to restart with the current binary",
		})
		return results
	}
	running := string(bytes.TrimSpace(data))
	if running == Version || running == "" {
		results = append(results, checkResult{
			name:   "version",
			ok:     true,
			detail: running,
		})
	} else {
		results = append(results, checkResult{
			name:   "version",
			ok:     false,
			detail: fmt.Sprintf("running %s, binary is %s", running, Version),
			fix:    "run `plumb stop` then reconnect to reload the current binary",
		})
	}
	return results
}

// checkLSPs reports install and runtime status for all configured language servers.
// Enabled languages with a missing binary are failures. Disabled languages are
// informational regardless of install status.
// For the Java language server, a separate Java runtime check is always included.
func checkLSPs(ws string) []checkResult {
	cfg, err := config.Load()
	if err != nil {
		return []checkResult{{
			name:   "config",
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

	// Stable order: go first, then alphabetical.
	order := []string{"go"}
	for lang := range cfg.LSP {
		if lang != "go" {
			order = append(order, lang)
		}
	}

	var results []checkResult
	for _, lang := range order {
		lspCfg, ok := cfg.LSP[lang]
		if !ok || lspCfg.Command == "" {
			continue
		}
		results = append(results, checkLSPBinary(lang, lspCfg))
		if lang == "java" {
			results = append(results, checkJavaRuntime())
		}
	}
	return results
}

func checkLSPBinary(lang string, lspCfg config.LSPConfig) checkResult {
	path, lookErr := exec.LookPath(lspCfg.Command)
	if lookErr != nil {
		if lspCfg.Enabled {
			return checkResult{
				name:   lang,
				ok:     false,
				detail: lspCfg.Command + " not found on PATH",
				fix:    "install " + lspCfg.Command + " and ensure it is on your PATH",
			}
		}
		return checkResult{
			name:   lang,
			ok:     true,
			detail: lspCfg.Command + " not installed — optional, enable in config.toml to activate",
		}
	}

	out, _ := commandOutputTimeout(path, "--version")
	version := strings.TrimSpace(string(out))
	detail := contractConfigPath(path)
	if version != "" {
		detail += "  " + version
	} else {
		detail += "  (version unknown)"
	}
	if !lspCfg.Enabled {
		detail += "  (disabled — enable in config.toml to activate)"
	}
	return checkResult{name: lang, ok: true, detail: detail}
}

// checkJavaRuntime checks that a Java 21+ runtime is on PATH.
// This is always shown in the Language Servers section when java is configured,
// regardless of whether jdtls itself is installed, because a missing or outdated
// JVM is the most common reason jdtls fails to start.
func checkJavaRuntime() checkResult {
	path, err := exec.LookPath("java")
	if err != nil {
		return checkResult{
			name:   "java runtime",
			ok:     false,
			detail: "java not found on PATH",
			fix:    "install Java 21+ (SDKMAN: sdk install java 21-tem, or your OS package manager)",
		}
	}
	out, err := commandOutputTimeout(path, "--version")
	if err != nil || len(out) == 0 {
		return checkResult{
			name:   "java runtime",
			ok:     true,
			detail: contractConfigPath(path) + "  (version unknown)",
		}
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	major := parseJavaMajorVersion(firstLine)
	detail := contractConfigPath(path) + "\n" + firstLine
	if major > 0 && major < 21 {
		return checkResult{
			name:   "java runtime",
			ok:     false,
			detail: fmt.Sprintf("%s  (Java %d — 21+ required by jdtls)", contractConfigPath(path), major),
			fix:    "upgrade to Java 21+",
		}
	}
	return checkResult{name: "java runtime", ok: true, detail: detail}
}

// parseJavaMajorVersion extracts the major version integer from a java --version
// first line, e.g. "openjdk 21.0.3 ..." → 21, "java 17.0.1 ..." → 17.
func parseJavaMajorVersion(versionLine string) int {
	for f := range strings.FieldsSeq(versionLine) {
		f = strings.Trim(f, "\"")
		// Old-style "1.8.0_292" — major is the component after "1."
		if strings.HasPrefix(f, "1.") && strings.Count(f, ".") >= 2 {
			if n, err := strconv.Atoi(strings.SplitN(f[2:], ".", 2)[0]); err == nil {
				return n
			}
		}
		// Modern: "21.0.3", "17.0.1" — major is the first component
		if n, err := strconv.Atoi(strings.SplitN(f, ".", 2)[0]); err == nil && n >= 8 {
			return n
		}
	}
	return 0
}

func commandOutputTimeout(name string, arg ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, arg...).Output()
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
				name:   c.name,
				ok:     false,
				detail: "cannot locate config: " + err.Error(),
			})
			continue
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			results = append(results, checkResult{
				name:   c.name,
				ok:     false,
				detail: "not installed or config not found",
				fix:    fmt.Sprintf("install %s, then run `plumb setup %s`", c.name, setupCmdName(c.name)),
			})
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			results = append(results, checkResult{
				name:   c.name,
				ok:     false,
				detail: "cannot read config: " + err.Error(),
			})
			continue
		}
		if strings.Contains(string(data), "plumb") {
			results = append(results, checkResult{
				name:   c.name,
				ok:     true,
				detail: contractConfigPath(path),
			})
		} else {
			results = append(results, checkResult{
				name:   c.name,
				ok:     false,
				detail: "config exists but plumb is not registered",
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
			detail: "not present (using compiled defaults)",
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

// checkStatsDB verifies the global stats DB is readable.
func checkStatsDB(ws string) []checkResult {
	dbPath := stats.DBPathFor()
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return []checkResult{{
			name:   "stats db",
			ok:     true,
			detail: "not present yet (created on first tool call)",
		}}
	}
	db, err := stats.OpenReadOnly()
	if err != nil {
		return []checkResult{{
			name:   "stats db",
			ok:     false,
			detail: err.Error(),
			fix:    "the DB may be corrupt — remove " + contractConfigPath(dbPath) + " to reset",
		}}
	}
	filter := stats.Filter{}
	if ws != "" {
		filter.Workspace = ws
	}
	total := db.TotalCalls(filter)
	db.Close()
	return []checkResult{{
		name:   "stats db",
		ok:     true,
		detail: fmt.Sprintf("%s  (%d calls recorded)", contractConfigPath(dbPath), total),
	}}
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
