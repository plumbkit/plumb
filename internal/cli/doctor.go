package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/tui"
)

// The doctor checks are split across files by concern: daemon / MCP-client /
// config / stats checks in doctor_checks.go; language-server checks in
// doctor_lsp.go; topology-index checks in doctor_topology.go. This file holds
// the command, the result model, and the rendering framework.

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
	return silentExitError{}
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
