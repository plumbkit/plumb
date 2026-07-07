package tui

// model_settings_commands_render.go — the rendering half of the Commands tab
// (the policy toggles, the side-by-side list | detail panes) plus the shell-word
// split/join the exec editor uses. Split from model_settings_commands.go (which
// holds state, persistence, and key handling) to keep each file under the
// ~600-line cap.

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/plumbkit/plumb/internal/config"
)

// settingsRowsLines returns the scrollable rows-pane lines for the active tab:
// the Commands tab's custom two-pane view, or the flat settingItem rows for
// every other tab.
func (m Model) settingsRowsLines(rowsW int) []string {
	if m.settingsTab == settingsTabCommands {
		return m.renderCommandsLines(rowsW)
	}
	return m.settingsDisplayLines(rowsW)
}

// renderCommandsLines builds the Commands tab body: the [commands] policy toggles,
// then a side-by-side list | detail editor for the allow-list.
func (m Model) renderCommandsLines(rowsW int) []string {
	leftW := min(max(rowsW/3, 16), 30)
	rightW := max(rowsW-leftW-3, 12)
	panes := joinCommandPanes(m.cmdListPaneLines(leftW), m.cmdDetailPaneLines(rightW), leftW, rightW)
	out := make([]string, 0, 6+len(panes))
	out = append(out,
		settingsHeaderDisplay("Policy (execute_shell_command)", rowsW, false),
		m.cmdToggleLine(0, "allow_shell", onOff(m.commandPolicy.AllowShell)),
		m.cmdToggleLine(1, "require_sandbox", onOff(m.commandPolicy.RequireSandbox)),
		m.cmdToggleLine(2, "deny_network", onOff(m.commandPolicy.DenyNetwork)),
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

// cmdExecDisplay renders the argv as a shell-quoted line, or "(unset)" when empty.
func cmdExecDisplay(argv []string) string {
	if len(argv) == 0 {
		return "(unset)"
	}
	return shellJoin(argv)
}

// shellSplit parses a command line into an argv, honouring single and double
// quotes so an argument may contain spaces ('a b' or "a b" → one arg). It is not
// a full shell parser (no escapes, no variable expansion) — just enough for the
// exec editor to round-trip an argument with a space.
func shellSplit(s string) []string {
	var args []string
	var cur strings.Builder
	inArg := false
	var quote rune
	flush := func() {
		if inArg {
			args = append(args, cur.String())
			cur.Reset()
			inArg = false
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			inArg = true
		case r == '\'' || r == '"':
			quote = r
			inArg = true
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	flush()
	return args
}

// shellJoin renders an argv back to an editable line, quoting any argument that
// is empty or contains whitespace/quotes so shellSplit round-trips it.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		switch {
		case a == "" || strings.ContainsAny(a, " \t\""):
			// Prefer single quotes; fall back to double quotes if it holds a single quote.
			if strings.Contains(a, "'") {
				parts[i] = "\"" + a + "\""
			} else {
				parts[i] = "'" + a + "'"
			}
		case strings.Contains(a, "'"):
			parts[i] = "\"" + a + "\""
		default:
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
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
