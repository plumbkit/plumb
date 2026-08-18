package cli

import (
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

// The interactive bare-`plumb setup` picker: one row per client, space to
// toggle, enter to apply. A registered client arrives checked; unchecking it
// flips to an explicit, warn-coloured uninstall that must be pressed again to
// back out of — the destructive direction is never a default. Applying reuses
// the bulk engine for registrations (so a re-register also repoints at the
// current binary) and the --uninstall writers for removals, then reports every
// outcome in one grouped table.

// setupPickerAction is what enter will do to one row.
type setupPickerAction int

const (
	setupKeep      setupPickerAction = iota // leave the client as is
	setupRegister                           // register (and repoint) plumb
	setupUninstall                          // remove plumb
)

// pickerRow is one client in the picker, pre-classified from the filesystem.
type pickerRow struct {
	target     setupTarget
	installed  bool // a config file exists, or the target's installedFn vouches
	registered bool // plumb's entry is present in at least one managed path
	action     setupPickerAction
}

func runSetupPicker() error {
	PrintLogo()

	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	final, err := runSetupPickerSelector(newPickerRows())
	if err != nil {
		return fmt.Errorf("client picker: %w", err)
	}
	if !final.applied || pickerChanges(final.rows) == 0 {
		fmt.Println("\nNo changes — nothing selected.")
		return nil
	}

	tui.RebuildStyles()
	applyPickerRows(final.rows, plumbBin)
	return nil
}

// newPickerRows classifies every client from its managed config paths: present
// paths (or an installedFn that vouches) mean installed; any path holding a
// plumb entry means registered.
func newPickerRows() []pickerRow {
	rows := make([]pickerRow, 0, 20)
	for _, c := range allSetupClients() {
		row := pickerRow{target: c}
		if paths, err := resolveTargetPaths(c); err == nil {
			for _, p := range paths {
				if _, err := os.Stat(p); err == nil {
					row.installed = true
				}
				if clientHasPlumb(c, p) {
					row.registered = true
				}
			}
		}
		if c.installedFn != nil && c.installedFn() {
			row.installed = true
		}
		rows = append(rows, row)
	}
	return rows
}

// pickerChanges counts rows carrying an action.
func pickerChanges(rows []pickerRow) int {
	n := 0
	for _, r := range rows {
		if r.action != setupKeep {
			n++
		}
	}
	return n
}

// applyPickerRows runs the selection: registrations through the bulk engine
// (which repoints an existing entry at plumbBin as it registers), removals
// through the --uninstall writers, everything reported in one grouped table.
func applyPickerRows(rows []pickerRow, plumbBin string) {
	t := render.NewGroupedTable(tui.SepStyle, tui.HintStyle, "Client", "Status", "Config")
	var skillNotes []string
	for _, r := range rows {
		switch r.action {
		case setupRegister:
			clientRows, _ := refreshClient(r.target, plumbBin, true)
			t.NextGroup()
			for _, cr := range clientRows {
				t.Row(cr.name, statusStyle(cr.status).Render(cr.status), cr.detail)
			}
		case setupUninstall:
			t.NextGroup()
			skillNotes = append(skillNotes, uninstallPickerRows(t, r.target)...)
		}
	}
	fmt.Println(t.Render())
	for _, r := range rows {
		if r.action == setupRegister {
			printSkillsDriftHint(r.target)
		}
	}
	for _, note := range skillNotes {
		fmt.Println(note)
	}
}

// uninstallPickerRows removes plumb from one target, appending one table row
// per managed path (refreshClient's shape), and returns a note per client
// whose plumb-installed skills were taken with it.
func uninstallPickerRows(t *render.GroupedTable, c setupTarget) []string {
	paths, err := resolveTargetPaths(c)
	if err != nil {
		t.Row(c.name, statusStyle("error").Render("error"), err.Error())
		return nil
	}
	removedAny := false
	for i, cfgPath := range paths {
		removed, err := c.outFn(cfgPath)
		status, detail := "not registered", render.ContractPath(cfgPath)
		switch {
		case err != nil:
			status, detail = "error", err.Error()
		case removed:
			removedAny = true
			status = "unregistered"
		}
		name := ""
		if i == 0 {
			name = c.name
		}
		t.Row(name, statusStyle(status).Render(status), detail)
	}
	if removedAny && c.skillsDirFn != nil {
		if dir, err := c.skillsDirFn(); err == nil {
			if removed, _ := removePlumbSkills(dir); len(removed) > 0 {
				return []string{fmt.Sprintf("Removed %d plumb skill(s) from %s", len(removed), render.ContractPath(dir))}
			}
		}
	}
	return nil
}

// runSetupPickerSelector runs the picker program and returns the final model.
func runSetupPickerSelector(rows []pickerRow) (setupPickerModel, error) {
	finalModel, err := tea.NewProgram(setupPickerModel{rows: rows}).Run()
	if err != nil {
		return setupPickerModel{}, err
	}
	m, ok := finalModel.(setupPickerModel)
	if !ok {
		return setupPickerModel{}, nil
	}
	return m, nil
}

type setupPickerModel struct {
	rows    []pickerRow
	cursor  int
	applied bool
}

func (m setupPickerModel) Init() tea.Cmd { return nil }

func (m setupPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "up", "k":
			m.cursor = (m.cursor + len(m.rows) - 1) % len(m.rows)
		case "down", "j":
			m.cursor = (m.cursor + 1) % len(m.rows)
		case " ":
			m.rows[m.cursor] = togglePickerRow(m.rows[m.cursor])
		case "a", "A":
			// Register-all never creates an uninstall: it only lifts the
			// unregistered rows, leaving whatever the user chose untouched.
			for i := range m.rows {
				if !m.rows[i].registered && m.rows[i].action == setupKeep {
					m.rows[i].action = setupRegister
				}
			}
		case "enter":
			m.applied = true
			return m, tea.Quit
		case "q", "esc", "ctrl+c":
			m.applied = false
			return m, tea.Quit
		}
	}
	return m, nil
}

// togglePickerRow flips a row between its neutral state and its one
// meaningful action: register for a client plumb is absent from, uninstall for
// one it is in. Pressing space again returns to neutral, so the destructive
// direction is always an explicit, visible choice.
func togglePickerRow(row pickerRow) pickerRow {
	if row.registered {
		if row.action == setupUninstall {
			row.action = setupKeep
		} else {
			row.action = setupUninstall
		}
		return row
	}
	if row.action == setupRegister {
		row.action = setupKeep
	} else {
		row.action = setupRegister
	}
	return row
}

func (m setupPickerModel) View() tea.View {
	return tea.NewView(renderSetupPicker(m))
}

// renderSetupPicker draws the checklist: the ❯ cursor marks the active row,
// the checkbox carries the row's resulting state, and the trailing tag says
// what that state is — a pending uninstall is the only warn-coloured tag.
func renderSetupPicker(m setupPickerModel) string {
	tui.RebuildStyles()
	nameW := 0
	for _, r := range m.rows {
		if w := lipgloss.Width(r.target.name); w > nameW {
			nameW = w
		}
	}

	var b strings.Builder
	b.WriteString(tui.ItemStyle.Render("Select clients — space toggles, enter applies.") + "\n\n")
	for i, r := range m.rows {
		cursor := "  "
		if i == m.cursor {
			cursor = "❯ "
		}
		var checkbox, tag string
		switch {
		case r.action == setupUninstall:
			checkbox = tui.WarnStyle.Render("[✗]")
			tag = tui.WarnStyle.Render("uninstall")
		case r.action == setupRegister:
			checkbox = tui.OkStyle.Render("[✓]")
			tag = tui.SelectedStyle.Render("will register")
		case r.registered:
			checkbox = tui.OkStyle.Render("[✓]")
			tag = tui.MutedStyle.Render("registered")
		case r.installed:
			checkbox = tui.MutedStyle.Render("[ ]")
			tag = tui.MutedStyle.Render("not registered")
		default:
			checkbox = tui.MutedStyle.Render("[ ]")
			tag = tui.MutedStyle.Render("not installed")
		}
		name := tui.ItemStyle.Render(r.target.name)
		if i == m.cursor {
			name = tui.SelectedStyle.Render(r.target.name)
		}
		b.WriteString("  " + cursor + checkbox + " " + render.PadRight(name, nameW) + "  " + tag + "\n")
	}
	b.WriteString("\n" + tui.HintStyle.Render("  space toggle · a register all · enter apply · q quit") + "\n")
	return b.String()
}
