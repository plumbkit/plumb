package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/golimpio/plumb/internal/memory"
)

// memDetailRow renders a key/value pair with a 2-space gap. Keys are padded to
// 4 chars so Name/Desc/Size align; longer keys (Paths) get 1 space gap.
func memDetailRow(k, v string) string {
	const kw = 4
	pad := max(kw-len(k), 0)
	return "  " + KeyStyle.Width(0).Render(k) + strings.Repeat(" ", pad+2) + ValStyle.Render(v)
}

func (m Model) memoryLeftLines() []string {
	lf := m.focusPanel == focusSessions

	var titleStyle lipgloss.Style
	if lf {
		titleStyle = PanelHeaderStyle
	} else {
		titleStyle = PanelHeaderFadedStyle
	}
	titleText := fmt.Sprintf(" Memories (%d)", len(m.memories))

	lines := []string{titleStyle.Render(titleText), ""}
	if len(m.memories) == 0 {
		msg := " No memories in this workspace."
		if lf {
			lines = append(lines, MutedStyle.Render(msg))
		} else {
			lines = append(lines, InactiveStyle.Render(msg))
		}
		return lines
	}

	const descPrefix = "    ╰─ "
	const descIndent = "       "
	prefixW := len([]rune(descPrefix)) // 7
	// Subtract 1 extra so ansi.Truncate in renderBodySection (threshold = leftWidth-1)
	// never fires on these lines.
	availW := max(m.leftWidth-prefixW-1, 4)

	for i, mem := range m.memories {
		selected := i == m.memoryCursor
		indicator := "○"
		if selected {
			indicator = "❯"
		}
		firstLine := " " + indicator + " " + mem.Name

		desc := mem.Description
		if desc == "" {
			desc = "(no description)"
		}
		descRunes := []rune(desc)

		var line2text, line3text string
		if len(descRunes) <= availW {
			line2text = string(descRunes)
		} else {
			line2text = string(descRunes[:availW])
			rest := descRunes[availW:]
			if len(rest) > availW {
				rest = append(rest[:availW-1], '…')
			}
			line3text = string(rest)
		}

		secondLine := descPrefix + line2text
		thirdLine := descIndent + line3text

		lines = append(lines, leftMemoryRowLines(firstLine, secondLine, thirdLine, selected, lf)...)
		lines = append(lines, "")
	}
	return lines
}

func leftMemoryRowLines(firstLine, secondLine, thirdLine string, selected, lf bool) []string {
	if selected {
		return []string{
			SelectedStyle.Render(firstLine),
			SelectedStyle.Render(secondLine),
			SelectedStyle.Render(thirdLine),
		}
	}
	if lf {
		return []string{
			ItemStyle.Render(firstLine),
			MutedStyle.Render(secondLine),
			MutedStyle.Render(thirdLine),
		}
	}
	return []string{
		FadedStyle.Render(firstLine),
		FadedStyle.Render(secondLine),
		FadedStyle.Render(thirdLine),
	}
}

func (m *Model) memoryRightLines(rw int) []string {
	rf := m.focusPanel != focusSessions
	var headerStyle lipgloss.Style
	if rf {
		headerStyle = PanelHeaderStyle
	} else {
		headerStyle = PanelHeaderFadedStyle
	}

	lines := []string{headerStyle.Render(" Memory Detail"), ""}

	if len(m.memories) == 0 {
		lines = append(lines, "  "+MutedStyle.Render("No memories in this workspace."))
		return lines
	}

	mem := m.memories[m.memoryCursor]

	lines = append(lines, memDetailRow("Name", mem.Name))
	if mem.Description != "" {
		lines = append(lines, memDetailRow("Desc", mem.Description))
	}
	lines = append(lines, memDetailRow("Size", fmt.Sprintf("%d bytes", mem.SizeBytes)))
	if len(mem.Paths) > 0 {
		lines = append(lines, memDetailRow("Paths", strings.Join(mem.Paths, ", ")))
	}
	lines = append(lines, "")
	lines = append(lines, "  "+SepStyle.Render(strings.Repeat("┄", max(rw-3, 1))))
	lines = append(lines, "")

	body := m.currentMemoryBody()
	if body == "" {
		lines = append(lines, "  "+MutedStyle.Render("(empty)"))
		return lines
	}
	// 2-space left margin + 2-space right margin = 4 chars reserved.
	bodyW := max(rw-4, 10)
	for srcLine := range strings.SplitSeq(body, "\n") {
		runes := []rune(srcLine)
		if len(runes) == 0 {
			lines = append(lines, "")
			continue
		}
		for len(runes) > 0 {
			n := min(len(runes), bodyW)
			lines = append(lines, "  "+DetailStyle.Render(string(runes[:n])))
			runes = runes[n:]
		}
	}
	return lines
}

func (m *Model) currentMemoryBody() string {
	if len(m.memories) == 0 {
		return ""
	}
	name := m.memories[m.memoryCursor].Name
	if m.memoryBodyCacheName == name && m.memoryBodyCache != "" {
		return m.memoryBodyCache
	}
	ws := m.dashProjectFolder
	if ws == "" {
		return ""
	}
	body, err := memory.Read(ws, name)
	if err != nil {
		body = "(error loading memory: " + err.Error() + ")"
	}
	m.memoryBodyCache = body
	m.memoryBodyCacheName = name
	return body
}

func (m *Model) refreshMemories() {
	ws := m.dashProjectFolder
	if ws == "" {
		m.memories = nil
		return
	}
	mems, err := memory.List(ws)
	if err != nil {
		m.memories = nil
		return
	}
	m.memories = mems
	if m.memoryCursor >= len(m.memories) && m.memoryCursor > 0 {
		m.memoryCursor = len(m.memories) - 1
	}
	// Invalidate body cache if the selected memory disappeared.
	if m.memoryBodyCacheName != "" {
		found := false
		for _, mem := range m.memories {
			if mem.Name == m.memoryBodyCacheName {
				found = true
				break
			}
		}
		if !found {
			m.memoryBodyCache = ""
			m.memoryBodyCacheName = ""
		}
	}
}

func (m *Model) selectMemoryAtBodyRow(row int) {
	if row < 1 || len(m.memories) == 0 {
		return
	}
	// Each memory occupies 4 rows: name, desc-line1, desc-line2, blank.
	idx := (row - 1) / 4
	if idx < 0 || idx >= len(m.memories) {
		return
	}
	m.memoryCursor = idx
	m.memoryBodyCache = ""
	m.memoryBodyCacheName = ""
	m.focusPanel = focusSessions
	m.rightScroll = 0
}
