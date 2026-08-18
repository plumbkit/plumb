package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/reflow/wordwrap"
	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/tui"
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
	// subOf names the parent row this result renders as a branch under; empty
	// means a top-level row. The link is explicit rather than parsed out of
	// names because sections with parenthesised rows of their own (Language
	// Servers' "go (live)") must never fold. jsonCheckResult maps its own
	// fields, so the --json schema is unaffected.
	subOf string
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

	failures, warnings := 0, 0
	for _, s := range doctorSections(ws) {
		checks := runSection(s)
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

// doctorSection is one titled group of checks. A section marked omitWhenEmpty
// prints nothing at all — no header, no rows, no blank lines — when its checks
// produce no results.
type doctorSection struct {
	title         string
	run           func() []checkResult
	omitWhenEmpty bool
}

// doctorSections is the SINGLE source of truth for which checks run and in what
// order. Both the human and the --json path consume it, so a new section cannot
// appear in one and be silently missing from the other — the two lists used to
// be declared separately, and the "Dev Tools" section added with this change
// would have been reported by `plumb doctor` but absent from `plumb doctor
// --json`, which is exactly the kind of drift no one notices until a script
// disagrees with the terminal.
func doctorSections(ws string) []doctorSection {
	return []doctorSection{
		{"Daemon", checkDaemon, false},
		{"Language Servers", func() []checkResult { return checkLSPs(ws) }, false},
		{"LSP Live", checkActiveLSPProcesses, true},
		{"MCP Clients", checkMCPClients, false},
		{"Configuration", func() []checkResult { return checkConfigs(ws) }, false},
		{"Dev Tools", checkDevTools, false},
		{"Integrations", func() []checkResult { return checkRastro(ws) }, false},
		{"Data", func() []checkResult { return checkStatsDB(ws) }, false},
		{"Indexing", func() []checkResult { return checkTopology(ws) }, false},
	}
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
	sections := doctorSections(ws)
	all := make([]checkResult, 0, len(sections)*3)
	for _, s := range sections {
		all = append(all, s.run()...)
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

// runSection runs a section's checks under the working indicator first, then
// prints the section — header, blank line, rows, trailing blank — only when it
// has rows or is not marked omitWhenEmpty. The header lands after the checks
// rather than before so an omitted section leaves nothing behind; the spinner
// line is self-clearing, which makes the reorder invisible where it does print.
func runSection(s doctorSection) []checkResult {
	stopWorking := startWorkingIndicator()
	checks := s.run()
	stopWorking()
	if s.omitWhenEmpty && len(checks) == 0 {
		return checks
	}
	fmt.Println(tui.HintStyle.Render("● " + s.title))
	fmt.Println()
	printChecks(checks)
	fmt.Println()
	return checks
}

// printChecks prints checks aligned by name column. A check whose subOf names
// another row renders as an indented branch directly beneath that parent, in
// its original position among the other subs; the name column is sized by
// parent rows only, so a branch label never widens it.
func printChecks(checks []checkResult) {
	nameW := 0
	parents := make(map[string]bool, len(checks))
	for _, c := range checks {
		if c.subOf != "" {
			continue
		}
		parents[c.name] = true
		if len(c.name) > nameW {
			nameW = len(c.name)
		}
	}
	for _, c := range checks {
		if c.subOf != "" {
			continue
		}
		printCheck(c, nameW)
		for _, sub := range checks {
			if sub.subOf == c.name {
				printSubCheck(sub, nameW)
			}
		}
	}
	// A sub whose parent row is absent renders top-level rather than silently
	// disappearing: the MCP Clients section appends subs from producers that run
	// independently of the parent's check, and a dropped parent must not take the
	// sub's diagnostics with it.
	for _, c := range checks {
		if c.subOf != "" && !parents[c.subOf] {
			printCheck(c, nameW)
		}
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

// subDetailLine is one laid-out line of a branch detail: the text, its extra
// indent past the detail column (0 flush, 3 for a wrapped continuation), and
// whether it closes the detail with its own "╰─" glyph.
type subDetailLine struct {
	text  string
	off   int
	close bool
}

// printSubCheck renders one sub-check as a branch beneath its parent row. The
// glyph and label carry structure and print in the hint colour whatever the
// sub's status; the detail keeps the table's status colouring — muted on a
// clean pass, the attention colour throughout when the sub is not clean. A
// detail whose lines all fit keeps its stacked shape, closing on its own
// "╰─" so a multi-line sub reads as one unit; a line that does not fit
// flows as a hanging paragraph instead (see subDetailLines).
func printSubCheck(c checkResult, nameW int) {
	attention := !c.ok || c.warn
	detailStyle := tui.MutedStyle
	if attention {
		detailStyle = tui.WarnStyle
	}
	detailCol := 7 + nameW
	// Five leading spaces put the glyph under the parent's name column; "╰─ "
	// ends three columns later, so the label starts eight columns in and pads
	// out to the parent's detail column.
	pad := detailCol - 8 - len(subLabel(c))
	if pad < 1 {
		pad = 1
	}
	lines := subDetailLines(c.detail, detailCol, terminalWidth(os.Stdout))
	fmt.Printf("     %s%s%s\n",
		tui.HintStyle.Render("╰─ "+subLabel(c)),
		strings.Repeat(" ", pad),
		detailStyle.Render(lines[0].text))
	for _, line := range lines[1:] {
		prefix := strings.Repeat(" ", detailCol+line.off)
		if line.close {
			fmt.Printf("%s%s%s\n", prefix, tui.HintStyle.Render("╰─ "), detailStyle.Render(line.text))
			continue
		}
		fmt.Printf("%s%s\n", prefix, detailStyle.Render(line.text))
	}
	if attention && c.fix != "" {
		fmt.Printf("%s%s\n", strings.Repeat(" ", detailCol), tui.WarnStyle.Render("→ "+c.fix))
	}
}

// subDetailLines lays out a branch detail against the terminal width. While
// every stacked line fits, the detail keeps its shape — one flush line per
// stacked line, the last one closing on its own "╰─". The first line that
// does not fit turns the detail into a flowing paragraph with a three-column
// hanging indent, and nothing closes on a glyph — the shared indent already
// makes the block read as one unit.
func subDetailLines(detail string, detailCol, width int) []subDetailLine {
	limit := max(width-detailCol, 20)
	hangLimit := max(limit-3, 20)
	stacked := strings.Split(detail, "\n")
	out := make([]subDetailLine, 0, len(stacked))
	flowed := false
	for _, seg := range stacked {
		if !flowed && lipgloss.Width(seg) <= limit {
			out = append(out, subDetailLine{text: seg})
			continue
		}
		out = append(out, flowSubDetail(seg, limit, hangLimit, !flowed)...)
		flowed = true
	}
	if !flowed && len(out) > 1 {
		out[len(out)-1].close = true
	}
	return out
}

// flowSubDetail wraps one too-long stacked segment into hanging-paragraph
// lines. A " — `…`" suggestion separator becomes the first break — the dash
// is dropped and the command span starts its own flush line — so a span the
// user is meant to read (or copy) whole survives the wrap; the text after it
// and any later stacked segment are pure continuations at the hanging indent.
func flowSubDetail(seg string, limit, hangLimit int, firstFlush bool) []subDetailLine {
	if !firstFlush {
		wrapped := wrapLines(seg, hangLimit)
		out := make([]subDetailLine, 0, len(wrapped))
		for _, line := range wrapped {
			out = append(out, subDetailLine{text: line, off: 3})
		}
		return out
	}
	intro, span, rest := seg, "", ""
	if before, after, ok := strings.Cut(seg, " — `"); ok && strings.Contains(after, "`") {
		spanEnd := strings.Index(after, "`")
		intro = before
		span = "`" + after[:spanEnd+1]
		rest = strings.TrimPrefix(after[spanEnd+1:], " ")
	}
	var out []subDetailLine
	for i, line := range wrapLines(intro, limit) {
		out = append(out, subDetailLine{text: line, off: hangOff(i)})
	}
	if span != "" {
		for i, line := range wrapLines(span, limit) {
			out = append(out, subDetailLine{text: line, off: hangOff(i)})
		}
	}
	if rest != "" {
		for _, line := range wrapLines(rest, hangLimit) {
			out = append(out, subDetailLine{text: line, off: 3})
		}
	}
	return out
}

// hangOff keeps a wrapped block's first line flush and indents its
// continuations onto the hanging indent.
func hangOff(i int) int {
	if i == 0 {
		return 0
	}
	return 3
}

// wrapLines word-wraps one plain-text segment; empty input stays one empty
// line so a blank stacked line still renders.
func wrapLines(s string, limit int) []string {
	return strings.Split(wordwrap.String(s, limit), "\n")
}

// subLabel derives the branch label from a sub's "<parent> (<label>)" name —
// "Claude Desktop (extra profiles)" branches as "Extra profiles". The full
// name stays on the result for --json, so the label is a rendering concern
// here rather than another field every producer must keep in sync.
func subLabel(c checkResult) string {
	prefix := c.subOf + " ("
	if strings.HasPrefix(c.name, prefix) && strings.HasSuffix(c.name, ")") {
		if s := c.name[len(prefix) : len(c.name)-1]; s != "" {
			return strings.ToUpper(s[:1]) + s[1:]
		}
	}
	return c.name
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

// stdoutIsTerminal reports whether stdout is an interactive terminal.
// term.IsTerminal rather than a ModeCharDevice stat, which also counts
// /dev/null — a character device, but never a terminal.
func stdoutIsTerminal() bool {
	return term.IsTerminal(os.Stdout.Fd())
}
