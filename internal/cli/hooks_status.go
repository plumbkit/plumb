package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

// The reporting half of `plumb hooks`: one grouped table shared by the status
// command and both writers, so a hook reads the same way whether you are
// looking at it, installing it, or taking it away. It follows `plumb skills`'s
// layout — a block per client, the client's config path on the block's first
// row, and the per-item outcome in the status cell.

// runHooksStatus renders the read-only per-client, per-hook table. A client
// that does not register plumb is shown as "unregistered" with the fix on its
// own rows, rather than as a pile of missing hooks whose reason the reader has
// to guess.
func runHooksStatus(cmd *cobra.Command) error {
	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	report := newHookReport()
	for _, t := range hooksTargets() {
		path, _, states, err := hookPlan(t, plumbBin)
		if err != nil {
			report.clientError(t, err)
			continue
		}
		if !plumbRegisteredIn(t.setup) {
			report.unregistered(t, path)
			continue
		}
		report.group(t, path, states, statusAction)
	}
	report.render(nil, cmd)
	return nil
}

// hookAction maps a hook's state before an operation to the word that goes in
// the status cell, plus the detail that replaces the config path on that row.
type hookAction func(hookState) (status, detail string)

// statusAction reports the state as it is.
func statusAction(s hookState) (string, string) { return s.state, s.detail }

// installAction says what the install did, read from the state BEFORE the
// write — "installed", "updated" and "current" describe an action, where
// restating the end state three times would not.
func installAction(s hookState) (string, string) {
	switch s.state {
	case hookStateMissing:
		return "installed", ""
	case hookStateStale:
		return "updated", s.detail
	default:
		return "current", ""
	}
}

// uninstallAction says what the removal did.
func uninstallAction(s hookState) (string, string) {
	if s.state == hookStateMissing {
		return "not installed", ""
	}
	return "uninstalled", s.detail
}

// hookReport accumulates the grouped table and the notes printed under it.
type hookReport struct {
	table *render.GroupedTable
	notes []string
	rows  int
}

func newHookReport() *hookReport {
	tui.RebuildStyles()
	return &hookReport{
		table: render.NewGroupedTable(tui.SepStyle, tui.HintStyle, "Client", "Hook", "Status", "Config"),
	}
}

// group appends one client's block: a row per hook, the client name and config
// path carried only on the first row so the block reads as one unit.
func (r *hookReport) group(t hooksTarget, path string, states []hookState, action hookAction) {
	r.table.NextGroup()
	for i, s := range states {
		name, shown := t.name, render.ContractPath(path)
		if i > 0 {
			name, shown = "", ""
		}
		status, detail := action(s)
		if detail != "" {
			shown = detail
		}
		r.table.Row(name, s.entry.label, statusStyle(status).Render(status), shown)
		r.rows++
	}
}

// unregistered is the block for a client plumb is not registered in: the reason
// hooks are skipped, and the command that fixes it.
func (r *hookReport) unregistered(t hooksTarget, path string) {
	r.table.NextGroup()
	r.table.Row(t.name, "—", statusStyle("unregistered").Render("unregistered"), render.ContractPath(path))
	r.table.Row("", "", "", "hooks need plumb registered, run:")
	r.table.Row("", "", "", "`plumb setup "+t.setup.use+"`")
	r.rows++
}

// clientError keeps a failure visible without failing the whole run: one
// client's unreadable config must not hide the others' state.
func (r *hookReport) clientError(t hooksTarget, err error) {
	r.table.NextGroup()
	r.table.Row(t.name, "—", statusStyle("error").Render("error"), err.Error())
	r.rows++
}

func (r *hookReport) note(lines ...string) {
	r.notes = append(r.notes, lines...)
}

// render prints the table, then any notes, then the clients skipped in a sweep.
// verbsOf, when set, adds that command's subcommands under the table — bare `plumb hooks` is
// read-only, so the two commands that change anything are worth naming where
// the reader is already looking, rather than only in --help.
func (r *hookReport) render(skips []string, verbsOf *cobra.Command) {
	if r.rows == 0 {
		fmt.Println("No hook-capable clients to report on.")
		return
	}
	fmt.Println(r.table.Render())
	if verbsOf != nil {
		printHookVerbs(verbsOf)
	}
	if len(r.notes) > 0 {
		fmt.Println()
		fmt.Println(tui.HintStyle.Render("● Notes"))
		fmt.Println()
		for _, n := range dedupeStrings(r.notes) {
			fmt.Printf("  %s %s\n", tui.SepStyle.Render("┊"), tui.MutedStyle.Render(n))
		}
	}
	if len(skips) > 0 {
		fmt.Println()
		for _, s := range skips {
			fmt.Println(tui.MutedStyle.Render(s))
		}
	}
}

// printHookVerbs lists the two writers under the read-only table, in the same
// shape `--help` uses — heading, bold name, muted description — with the text
// taken from the cobra commands themselves so the two can never drift apart.
// Bare `plumb hooks` changes nothing, so the commands that do are worth naming
// where the reader is already looking.
func printHookVerbs(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	// The command comes from cobra rather than the package vars: naming those
	// here would close an initialisation cycle (the command's RunE reaches this
	// function), and walking the real command tree also means a verb added later
	// appears without anyone remembering to list it twice.
	verbs := make([]*cobra.Command, 0, len(cmd.Commands()))
	width := 0
	for _, c := range cmd.Commands() {
		if c.Hidden || !c.IsAvailableCommand() {
			continue
		}
		verbs = append(verbs, c)
		if n := len(c.Name()); n > width {
			width = n
		}
	}
	if len(verbs) == 0 {
		return
	}
	fmt.Println()
	fmt.Println(tui.ItemStyle.Render("Available Commands:"))
	for _, c := range verbs {
		name := fmt.Sprintf("  %-*s", width+2, c.Name())
		fmt.Printf("%s%s\n", tui.HintStyle.Bold(true).Render(name), tui.MutedStyle.Render(c.Short))
	}
	fmt.Println()
	fmt.Printf("%s\n", tui.MutedStyle.Render(fmt.Sprintf("Use \"%s [command] --help\" for more information about a command.", cmd.CommandPath())))
}

// dedupeStrings keeps the first occurrence of each line. Two clients installed
// in one sweep can carry the same note, and printing it twice would read as a
// bug rather than as emphasis.
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
