package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/golimpio/plumb/internal/topology"
	"github.com/golimpio/plumb/internal/tui"
)

var (
	doctorWorkspace string
	doctorJSON      bool
)

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
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false,
		"Emit results as a JSON array instead of the ANSI table")
}

type checkResult struct {
	name   string
	ok     bool // false = failure (drives the exit code); a warning keeps ok=true
	warn   bool // ok=true but with a non-fatal caveat — rendered "!", never a failure
	detail string
	fix    string // one-line hint printed when the check is not a clean pass
}

func runDoctor(_ *cobra.Command, _ []string) error {
	ws := doctorWorkspace
	if ws == "" {
		if cwd, err := os.Getwd(); err == nil {
			ws = cwd
		}
	}
	if doctorJSON {
		return runDoctorJSON(ws)
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
		{"Indexing", func() []checkResult { return checkTopology(ws) }},
	}

	failures, warnings := 0, 0
	for _, s := range sections {
		checks := runSection(s.title, s.run)
		for _, c := range checks {
			switch {
			case !c.ok:
				failures++
			case c.warn:
				warnings++
			}
		}
	}

	if failures == 0 {
		msg := "All checks passed."
		if warnings > 0 {
			msg = fmt.Sprintf("All checks passed — %d warning(s), see notes above.", warnings)
		}
		fmt.Println(tui.OkStyle.Render(msg))
		return nil
	}
	fmt.Printf("%s  %d check(s) need attention — see hints above.\n",
		tui.WarnStyle.Render("✗"), failures)
	return fmt.Errorf("%d check(s) need attention", failures)
}

// jsonCheckResult is the JSON serialisation shape for a single doctor check.
type jsonCheckResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

// runDoctorJSON runs all doctor checks and writes results as a JSON array to
// stdout. Working indicators and section headers are suppressed. Exit code
// behaviour is unchanged: returns a non-nil error when any check fails.
func runDoctorJSON(ws string) error {
	runs := []func() []checkResult{
		checkDaemon,
		func() []checkResult { return checkLSPs(ws) },
		checkMCPClients,
		func() []checkResult { return checkConfigs(ws) },
		func() []checkResult { return checkStatsDB(ws) },
		func() []checkResult { return checkTopology(ws) },
	}
	all := make([]checkResult, 0, len(runs)*3)
	for _, run := range runs {
		all = append(all, run()...)
	}

	out := make([]jsonCheckResult, len(all))
	for i, c := range all {
		out[i] = jsonCheckResult{Name: c.name, OK: c.ok, Warn: c.warn, Detail: c.detail, Fix: c.fix}
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		return fmt.Errorf("encoding results: %w", err)
	}

	failures := 0
	for _, c := range all {
		if !c.ok {
			failures++
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d check(s) need attention", failures)
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
		printCheck(c, nameW)
	}
}

// printCheck renders one result. Failures (ok=false) get a "✗" marker, warnings
// (ok=true, warn=true) a "!"; both show the detail and fix in the attention
// colour. Clean passes get a "✓" and no fix line.
func printCheck(c checkResult, nameW int) {
	attention := !c.ok || c.warn
	marker := tui.OkStyle.Render("✓")
	switch {
	case !c.ok:
		marker = tui.WarnStyle.Render("✗")
	case c.warn:
		marker = tui.WarnStyle.Render("!")
	}
	name := fmt.Sprintf("%-*s", nameW, c.name)
	if attention {
		name = tui.WarnStyle.Render(name)
	}
	detailLines := strings.Split(c.detail, "\n")
	detail := detailLines[0]
	if attention {
		detail = tui.WarnStyle.Render(detail)
	}
	fmt.Printf("  %s  %s  %s\n", marker, name, detail)
	indent := strings.Repeat(" ", 7+nameW)
	for _, line := range detailLines[1:] {
		if attention {
			line = tui.WarnStyle.Render(line)
		}
		fmt.Printf("%s%s\n", indent, line)
	}
	if attention && c.fix != "" {
		fmt.Printf("%s%s\n", indent, tui.WarnStyle.Render("→ "+c.fix))
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

// checkTopology reports the health of the per-workspace topology index. It is a
// no-op pass when topology is disabled (it is on by default; this is the
// opted-out case). When enabled, it
// inspects the on-disk index read-only (without starting an indexer): a missing
// or empty index is a failure with a hint to run a daemon session.
func checkTopology(ws string) []checkResult {
	cfg, err := config.Load()
	if err != nil {
		return []checkResult{{
			name:   "topology",
			ok:     false,
			detail: err.Error(),
			fix:    "fix global config at " + contractConfigPath(config.GlobalConfigPath()),
		}}
	}
	if ws != "" {
		if merged, mErr := config.LoadProject(cfg, ws); mErr == nil {
			cfg = merged
		}
	}
	if !cfg.Topology.Enabled {
		return []checkResult{{
			name:   "topology",
			ok:     true,
			detail: "disabled ([topology] enabled = false — on by default)",
		}}
	}
	if ws == "" {
		return []checkResult{{
			name:   "topology",
			ok:     true,
			detail: "enabled (pass --workspace to inspect the index)",
		}}
	}
	return checkTopologyIndex(ws)
}

// checkTopologyIndex inspects the on-disk topology index for an enabled
// workspace. A missing or corrupt DB is a hard failure; the health of an index
// that does exist is classified by topologyIndexHealth.
func checkTopologyIndex(ws string) []checkResult {
	st, err := topology.StatusForWorkspace(ws)
	if err != nil {
		if os.IsNotExist(err) {
			return []checkResult{{
				name:   "topology",
				ok:     false,
				detail: "enabled but no index found",
				fix:    "open a plumb session in this workspace so the daemon builds the index",
			}}
		}
		return []checkResult{{
			name:   "topology",
			ok:     false,
			detail: err.Error(),
			fix:    "the index may be corrupt — remove " + contractConfigPath(topology.DBPath(ws)) + " to rebuild",
		}}
	}
	return []checkResult{topologyIndexHealth(st)}
}

// topologyIndexHealth classifies a topology Status whose DB exists. An index
// that has not finished its first pass is a non-fatal warning rather than a
// failure: a freshly enabled workspace inspected before the background indexer
// completes is healthy-but-pending, not broken, and `plumb doctor` must not
// emit a false negative (or a non-zero exit) during that window. The states:
//
//   - no file processed yet — cold start, warning;
//   - files seen but all errored/skipped — warning;
//   - files indexed but no symbols — warning (legitimate for a docs/config-only
//     tree; also the signature of a broken extractor, so it is surfaced);
//   - symbols present — pass.
func topologyIndexHealth(st topology.Status) checkResult {
	switch {
	case st.IndexedFiles == 0 && st.SkippedFiles == 0:
		return checkResult{
			name:   "topology",
			ok:     true,
			warn:   true,
			detail: "index is empty — initial indexing may still be in progress",
			fix:    "re-run once the first index completes; if it stays empty, check daemon.log",
		}
	case st.IndexedFiles == 0:
		return checkResult{
			name:   "topology",
			ok:     true,
			warn:   true,
			detail: fmt.Sprintf("no files indexed yet (%d skipped)", st.SkippedFiles),
			fix:    "check daemon.log for extractor errors",
		}
	case st.TotalNodes == 0:
		return checkResult{
			name:   "topology",
			ok:     true,
			warn:   true,
			detail: fmt.Sprintf("%d files indexed but no symbols extracted (expected for a docs/config-only tree)", st.IndexedFiles),
			fix:    "if this tree has source files, check daemon.log for extractor errors",
		}
	default:
		detail := fmt.Sprintf("%d files, %d nodes, %d edges, %s",
			st.IndexedFiles, st.TotalNodes, st.TotalEdges, humanBytes(st.DBSizeBytes))
		if len(st.Languages) > 0 {
			detail += "  [" + strings.Join(st.Languages, ", ") + "]"
		}
		return checkResult{name: "topology", ok: true, detail: detail}
	}
}

// humanBytes formats a byte count for one-line doctor output.
func humanBytes(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
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
