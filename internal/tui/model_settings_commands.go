package tui

// model_settings_commands.go — the Commands tab of the Settings screen
// (settingsTabCommands, 2nd position). It manages the [[command]] allow-list and
// the [commands] policy table for the selected scope, reusing the shared Scope
// column. Unlike the other tabs it does NOT use the flat settingItem rows: it
// renders its own view — two policy toggles above a side-by-side list | detail
// editor — and routes its own keys. Persistence mirrors the General tab (Global →
// config.Save, a workspace → a sparse .plumb/config.toml override) with one
// addition: a workspace-scope write also marks the workspace trusted, so a
// human-authored command is trusted-by-authorship (see applyScopedCommandsAt).
//
// Concurrency: TUI-thread only, like every other model field.

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/plumbkit/plumb/internal/config"
)

// commandsFocus identifies which sub-area of the Commands tab holds focus.
type commandsFocus int

const (
	cmdFocusToggles commandsFocus = iota // the two [commands] policy toggles
	cmdFocusList                         // the command-name list (left pane)
	cmdFocusDetail                       // the selected command's detail form (right pane)
)

// The Detail form's fields, in display order.
const (
	cmdFieldExec = iota
	cmdFieldWorkingDir
	cmdFieldTimeout
	cmdFieldAllowWrites
	cmdFieldDenyNetwork
	commandDetailFieldCount
)

// commandsToggleCount is the number of [commands] policy toggles.
const commandsToggleCount = 2

// cmdField sentinel values carried by the text editor for the name add/rename
// flows (the concrete field editors use the literal field names "exec" etc.).
const (
	cmdEditAdd  = "__add__"
	cmdEditName = "__name__"
)

// resetCommandsFocus parks the Commands tab back at the policy toggles, used when
// the section is (re)entered or the scope/tab changes so focus never lands on a
// stale command index.
func (m *Model) resetCommandsFocus() {
	m.commandsFocus = cmdFocusToggles
	m.commandsToggleCursor = 0
	m.commandsListCursor = 0
	m.commandsDetailCursor = 0
}

// refreshCommands reloads the effective command allow-list + policy for the
// current scope and clamps the cursors. Called from refreshSettingsItems so the
// tab tracks scope changes and saved edits without a separate call site.
func (m *Model) refreshCommands() {
	cfg := m.scopeConfig()
	m.commandsList = cfg.Commands
	m.commandPolicy = cfg.CommandPolicy
	if m.commandsListCursor >= len(m.commandsList) {
		m.commandsListCursor = max(len(m.commandsList)-1, 0)
	}
	if m.commandsListCursor < 0 {
		m.commandsListCursor = 0
	}
	if m.commandsDetailCursor >= commandDetailFieldCount {
		m.commandsDetailCursor = commandDetailFieldCount - 1
	}
}

// scopeConfig returns the resolved config for the current scope: the global
// snapshot in Global, or the project-merged config for a workspace (falling back
// to the global snapshot if the project config cannot be read).
func (m Model) scopeConfig() config.Config {
	cfg := m.settingsCfg
	if scope := m.currentScope(); !scope.global {
		if merged, err := config.LoadProject(m.settingsCfg, scope.folder); err == nil {
			cfg = merged
		}
	}
	return cfg
}

// --- persistence -----------------------------------------------------------

// applyScopedCommandsAt persists a Commands-tab change in the current scope,
// mirroring applyScopedAt. The one addition is auto-trust: a human editing a
// workspace's command allow-list or [commands] policy in the TUI is authoring it
// directly, so the workspace is marked trusted — otherwise the project-supplied
// command/policy would be ignored until `plumb trust`, since a project's
// .plumb/config.toml is an untrusted surface (a cloned repo ships one). Global
// scope needs no trust (the user's own config is trusted by definition).
func (m *Model) applyScopedCommandsAt(path []string, value any, apply func(*config.Config)) bool {
	scope := m.currentScope()
	if scope.global {
		if !m.persist(apply) {
			return false
		}
		apply(&m.settingsCfg)
		m.refreshSettingsItems()
		return true
	}
	if err := config.SetProjectValue(scope.folder, path, value); err != nil {
		m.settingsStatus = "save failed: " + err.Error()
		return false
	}
	if err := config.NewTrustStore().SetTrusted(scope.folder, true); err != nil {
		m.settingsStatus = "saved · trust update failed: " + err.Error()
	}
	m.pendingProjectReload = scope.folder
	m.refreshSettingsItems()
	return true
}

// commandsStatus formats the post-change status for the current scope.
func (m Model) commandsStatus(change string) string {
	if m.currentScope().global {
		return change + " · applied live"
	}
	return change + " · workspace override (trusted)"
}

// saveCommands persists the whole [[command]] allow-list for the current scope.
// The array is written as a unit (a sparse per-key write cannot address one
// entry of an array-of-tables).
func (m Model) saveCommands(cmds []config.CommandConfig, change string) Model {
	if m.applyScopedCommandsAt([]string{"command"}, cmds, func(c *config.Config) { c.Commands = cmds }) {
		m.settingsStatus = m.commandsStatus(change)
	}
	return m
}

// toggleCommandPolicy flips the policy toggle at idx (allow_shell / require_sandbox)
// and persists it in the current scope.
func (m Model) toggleCommandPolicy(idx int) Model {
	var (
		path   []string
		change string
		v      bool
		apply  func(*config.Config)
	)
	switch idx {
	case 0:
		v = !m.commandPolicy.AllowShell
		path = []string{"commands", "allow_shell"}
		change = "allow_shell " + onOff(v)
		apply = func(c *config.Config) { c.CommandPolicy.AllowShell = v }
	case 1:
		v = !m.commandPolicy.RequireSandbox
		path = []string{"commands", "require_sandbox"}
		change = "require_sandbox " + onOff(v)
		apply = func(c *config.Config) { c.CommandPolicy.RequireSandbox = v }
	default:
		return m
	}
	if m.applyScopedCommandsAt(path, v, apply) {
		m.settingsStatus = m.commandsStatus(change)
	}
	return m
}

// commitCommandField is invoked by the text editor's commit when it was opened
// for a Commands-tab field. It routes to the add / rename / set-field flows.
func (m Model) commitCommandField(field, input string) Model {
	switch field {
	case cmdEditAdd:
		return m.addCommand(input)
	case cmdEditName:
		return m.renameCommand(input)
	default:
		return m.setCommandField(field, input)
	}
}

// addCommand appends a new command with a placeholder exec (the name itself, so
// the allow-list stays valid — an empty exec is rejected on load) and drops the
// user straight into its Detail form.
func (m Model) addCommand(input string) Model {
	name := strings.TrimSpace(input)
	if name == "" {
		m.settingsStatus = "command name required"
		return m
	}
	if _, ok := config.FindCommand(m.commandsList, name); ok {
		m.settingsStatus = "command " + name + " already exists"
		return m
	}
	cmds := append(cloneCommands(m.commandsList), config.CommandConfig{Name: name, Exec: []string{name}})
	next := m.saveCommands(cmds, "added "+name)
	next.commandsListCursor = max(len(next.commandsList)-1, 0)
	next.commandsFocus = cmdFocusDetail
	next.commandsDetailCursor = cmdFieldExec
	return next
}

// renameCommand renames the selected command, refusing a blank or a name that
// collides with another entry.
func (m Model) renameCommand(input string) Model {
	name := strings.TrimSpace(input)
	if name == "" || m.commandsListCursor >= len(m.commandsList) {
		return m
	}
	cur := m.commandsList[m.commandsListCursor].Name
	if ex, ok := config.FindCommand(m.commandsList, name); ok && ex.Name != cur {
		m.settingsStatus = "command " + name + " already exists"
		return m
	}
	cmds := cloneCommands(m.commandsList)
	cmds[m.commandsListCursor].Name = name
	return m.saveCommands(cmds, "renamed → "+name)
}

// setCommandField updates one Detail field (exec / working_dir / timeout) of the
// selected command from the text-editor input.
func (m Model) setCommandField(field, input string) Model {
	if m.commandsListCursor >= len(m.commandsList) {
		return m
	}
	cmds := cloneCommands(m.commandsList)
	c := &cmds[m.commandsListCursor]
	switch field {
	case "exec":
		// The argv is edited as a single space-separated line; split on
		// whitespace. (First draft: no shell-style quoting — an argument that must
		// contain a space is not expressible here yet.)
		argv := strings.Fields(input)
		if len(argv) == 0 {
			m.settingsStatus = "exec must be a non-empty argv"
			return m
		}
		c.Exec = argv
	case "working_dir":
		c.WorkingDir = strings.TrimSpace(input)
	case "timeout":
		s := strings.TrimSpace(input)
		if s == "" {
			c.Timeout = config.Duration{}
		} else {
			d, err := time.ParseDuration(s)
			if err != nil {
				m.settingsStatus = "invalid timeout: " + err.Error()
				return m
			}
			c.Timeout = config.Duration{Duration: d}
		}
	default:
		return m
	}
	return m.saveCommands(cmds, c.Name+" · "+field+" updated")
}

// toggleCommandDetailBool flips the allow_writes / deny_network flag of the
// selected command.
func (m Model) toggleCommandDetailBool(field int) Model {
	if m.commandsListCursor >= len(m.commandsList) {
		return m
	}
	cmds := cloneCommands(m.commandsList)
	c := &cmds[m.commandsListCursor]
	var change string
	switch field {
	case cmdFieldAllowWrites:
		c.AllowWrites = !c.AllowWrites
		change = c.Name + " · allow_writes " + onOff(c.AllowWrites)
	case cmdFieldDenyNetwork:
		c.DenyNetwork = !c.DenyNetwork
		change = c.Name + " · deny_network " + onOff(c.DenyNetwork)
	default:
		return m
	}
	return m.saveCommands(cmds, change)
}

// deleteCommand removes the selected command and re-parks the cursor.
func (m Model) deleteCommand() Model {
	if m.commandsListCursor >= len(m.commandsList) {
		return m
	}
	name := m.commandsList[m.commandsListCursor].Name
	cmds := cloneCommands(m.commandsList)
	cmds = append(cmds[:m.commandsListCursor], cmds[m.commandsListCursor+1:]...)
	next := m.saveCommands(cmds, "removed "+name)
	if next.commandsListCursor >= len(next.commandsList) {
		next.commandsListCursor = max(len(next.commandsList)-1, 0)
	}
	if len(next.commandsList) == 0 {
		next.commandsFocus = cmdFocusList
	}
	return next
}

// cloneCommands deep-copies the allow-list (and each entry's Exec) so an edit
// never mutates the model's live slice before the save round-trips.
func cloneCommands(cmds []config.CommandConfig) []config.CommandConfig {
	out := make([]config.CommandConfig, len(cmds))
	for i, c := range cmds {
		c.Exec = append([]string(nil), c.Exec...)
		out[i] = c
	}
	return out
}

// --- key handling ----------------------------------------------------------

// handleCommandsKey routes a key within the Commands tab's rows pane (the Scope
// column and pane-width controls are handled upstream). It dispatches to the
// focused sub-area.
func (m Model) handleCommandsKey(key string) (Model, tea.Cmd) {
	switch m.commandsFocus {
	case cmdFocusToggles:
		return m.handleCommandsTogglesKey(key)
	case cmdFocusList:
		return m.handleCommandsListKey(key)
	case cmdFocusDetail:
		return m.handleCommandsDetailKey(key)
	default:
		return m, nil
	}
}

func (m Model) handleCommandsTogglesKey(key string) (Model, tea.Cmd) {
	switch key {
	case "up", "k":
		if m.commandsToggleCursor > 0 {
			m.commandsToggleCursor--
		}
	case "down", "j":
		if m.commandsToggleCursor < commandsToggleCount-1 {
			m.commandsToggleCursor++
		} else {
			m.commandsFocus = cmdFocusList // descend past the last toggle into the panes
		}
	case "enter", " ", "left", "right", "-", "+", "=":
		return m.afterSettingChange(m.toggleCommandPolicy(m.commandsToggleCursor), nil)
	}
	return m, nil
}

func (m Model) handleCommandsListKey(key string) (Model, tea.Cmd) {
	switch key {
	case "up", "k":
		if m.commandsListCursor > 0 {
			m.commandsListCursor--
		} else {
			m.commandsFocus = cmdFocusToggles
			m.commandsToggleCursor = commandsToggleCount - 1
		}
	case "down", "j":
		if m.commandsListCursor < len(m.commandsList)-1 {
			m.commandsListCursor++
		}
	case "right", "l", "enter", " ":
		if len(m.commandsList) > 0 {
			m.commandsFocus = cmdFocusDetail
			m.commandsDetailCursor = 0
		}
	case "a", "+":
		return m.openAddCommandEditor(), nil
	case "e":
		return m.openRenameCommandEditor(), nil
	case "d", "delete":
		return m.afterSettingChange(m.deleteCommand(), nil)
	}
	return m, nil
}

func (m Model) handleCommandsDetailKey(key string) (Model, tea.Cmd) {
	switch key {
	case "up", "k":
		if m.commandsDetailCursor > 0 {
			m.commandsDetailCursor--
		}
	case "down", "j":
		if m.commandsDetailCursor < commandDetailFieldCount-1 {
			m.commandsDetailCursor++
		}
	case "left", "h":
		m.commandsFocus = cmdFocusList
	case "enter", " ", "right", "-", "+", "=":
		return m.afterSettingChange(m.activateCommandDetail(), nil)
	}
	return m, nil
}

// activateCommandDetail opens the editor for a text field, or flips a bool field,
// of the selected command.
func (m Model) activateCommandDetail() Model {
	if m.commandsListCursor >= len(m.commandsList) {
		return m
	}
	c := m.commandsList[m.commandsListCursor]
	switch m.commandsDetailCursor {
	case cmdFieldExec:
		return m.openCommandFieldEditor("exec", c.Name+" · exec", strings.Join(c.Exec, " "))
	case cmdFieldWorkingDir:
		return m.openCommandFieldEditor("working_dir", c.Name+" · working_dir", c.WorkingDir)
	case cmdFieldTimeout:
		return m.openCommandFieldEditor("timeout", c.Name+" · timeout", cmdTimeoutInput(c.Timeout))
	case cmdFieldAllowWrites, cmdFieldDenyNetwork:
		return m.toggleCommandDetailBool(m.commandsDetailCursor)
	default:
		return m
	}
}

// openAddCommandEditor / openRenameCommandEditor / openCommandFieldEditor open the
// shared single-line text editor tagged with a cmdField, so its commit routes
// back through commitCommandField.
func (m Model) openAddCommandEditor() Model {
	ed := newTextEditor(0, "", "new command name", "")
	ed.cmdField = cmdEditAdd
	m.settingsTextEditor = ed
	return m
}

func (m Model) openRenameCommandEditor() Model {
	if m.commandsListCursor >= len(m.commandsList) {
		return m
	}
	cur := m.commandsList[m.commandsListCursor].Name
	ed := newTextEditor(0, "", "rename command", cur)
	ed.cmdField = cmdEditName
	m.settingsTextEditor = ed
	return m
}

func (m Model) openCommandFieldEditor(field, title, current string) Model {
	ed := newTextEditor(0, "", title, current)
	ed.cmdField = field
	m.settingsTextEditor = ed
	return m
}

// --- rendering -------------------------------------------------------------

// settingsRowsLines returns the scrollable rows-pane lines for the active tab:
// the Commands tab's custom two-pane view, or the flat settingItem rows for
// every other tab.
func (m Model) settingsRowsLines(rowsW int) []string {
	if m.settingsTab == settingsTabCommands {
		return m.renderCommandsLines(rowsW)
	}
	return m.settingsDisplayLines(rowsW)
}

// renderCommandsLines builds the Commands tab body: the two [commands] policy
// toggles, then a side-by-side list | detail editor for the allow-list.
func (m Model) renderCommandsLines(rowsW int) []string {
	leftW := min(max(rowsW/3, 16), 30)
	rightW := max(rowsW-leftW-3, 12)
	panes := joinCommandPanes(m.cmdListPaneLines(leftW), m.cmdDetailPaneLines(rightW), leftW, rightW)
	out := make([]string, 0, 5+len(panes))
	out = append(out,
		settingsHeaderDisplay("Policy", rowsW, false),
		m.cmdToggleLine(0, "allow_shell", onOff(m.commandPolicy.AllowShell)),
		m.cmdToggleLine(1, "require_sandbox", onOff(m.commandPolicy.RequireSandbox)),
		"",
		settingsHeaderDisplay("Allow-list", rowsW, false),
	)
	return append(out, panes...)
}

// cmdToggleLine renders one policy-toggle row, highlighted when the toggles hold
// focus and the cursor is on it.
func (m Model) cmdToggleLine(idx int, label, val string) string {
	ctrl := "[ " + val + " ]"
	if m.commandsFocus == cmdFocusToggles && m.commandsToggleCursor == idx {
		return " " + SelectedStyle.Render(fmt.Sprintf("❯ %-18s %s", label, ctrl))
	}
	return "   " + ItemStyle.Render(fmt.Sprintf("%-18s", label)) + " " + MutedStyle.Render(ctrl)
}

// cmdListPaneLines renders the left pane: the command names, the cursor row
// brightest when the list holds focus.
func (m Model) cmdListPaneLines(w int) []string {
	lines := []string{PanelHeaderFadedStyle.Render(fmt.Sprintf("Commands (%d)", len(m.commandsList)))}
	if len(m.commandsList) == 0 {
		return append(lines, MutedStyle.Render("(none — a to add)"))
	}
	focused := m.commandsFocus == cmdFocusList
	for i, c := range m.commandsList {
		name := truncate(c.Name, max(w-2, 1))
		switch {
		case i == m.commandsListCursor && focused:
			lines = append(lines, SelectedStyle.Render("❯ "+name))
		case i == m.commandsListCursor:
			lines = append(lines, ItemStyle.Render("❯ "+name))
		default:
			lines = append(lines, MutedStyle.Render("∙ ")+ItemStyle.Render(name))
		}
	}
	return lines
}

// cmdDetailPaneLines renders the right pane: the selected command's fields, the
// cursor row highlighted when the Detail form holds focus.
func (m Model) cmdDetailPaneLines(w int) []string {
	lines := []string{PanelHeaderFadedStyle.Render("Detail")}
	if m.commandsListCursor >= len(m.commandsList) {
		return append(lines, MutedStyle.Render("(no command selected)"))
	}
	c := m.commandsList[m.commandsListCursor]
	focused := m.commandsFocus == cmdFocusDetail
	fields := []struct{ label, val string }{
		{"exec", cmdExecDisplay(c.Exec)},
		{"working_dir", cmdDirDisplay(c.WorkingDir)},
		{"timeout", cmdTimeoutDisplay(c.Timeout)},
		{"allow_writes", onOff(c.AllowWrites)},
		{"deny_network", onOff(c.DenyNetwork)},
	}
	for i, f := range fields {
		row := truncate(fmt.Sprintf("%-12s %s", f.label, f.val), max(w-2, 1))
		if focused && i == m.commandsDetailCursor {
			lines = append(lines, SelectedStyle.Render("❯ "+row))
		} else {
			lines = append(lines, "  "+DetailStyle.Render(row))
		}
	}
	return lines
}

// joinCommandPanes lays the list and detail panes side by side, each fixed to its
// width (truncated, never wrapped) with a thin divider between.
func joinCommandPanes(left, right []string, leftW, rightW int) []string {
	n := max(len(left), len(right))
	out := make([]string, n)
	for i := range n {
		var l, r string
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		lc := lipgloss.NewStyle().Width(leftW).Render(ansi.Truncate(l, max(leftW-1, 1), "…"))
		rc := lipgloss.NewStyle().Width(rightW).Render(ansi.Truncate(r, max(rightW-1, 1), "…"))
		out[i] = " " + lc + SepStyle.Render("┆ ") + rc
	}
	return out
}

// cmdExecDisplay renders the argv as a space-joined line, or "(unset)" when empty.
func cmdExecDisplay(argv []string) string {
	if len(argv) == 0 {
		return "(unset)"
	}
	return strings.Join(argv, " ")
}

// cmdDirDisplay renders working_dir, defaulting an empty value to the root.
func cmdDirDisplay(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return "(workspace root)"
	}
	return dir
}

// cmdTimeoutDisplay renders the timeout, defaulting a zero value to the runner
// default.
func cmdTimeoutDisplay(d config.Duration) string {
	if d.Duration <= 0 {
		return "(default)"
	}
	return d.String()
}

// cmdTimeoutInput seeds the timeout editor: blank for the default, otherwise the
// human-friendly duration string.
func cmdTimeoutInput(d config.Duration) string {
	if d.Duration <= 0 {
		return ""
	}
	return d.String()
}
