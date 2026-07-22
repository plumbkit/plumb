package tui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/memory"
)

// memWorkspace is one row in the Memory section's Workspaces pane. Memory is
// per-workspace, not per-session, so two sessions on the same folder collapse to
// a single entry (Sessions == 2). Launch is set for the workspace the TUI was
// opened in, which appears even when no session is currently attached there.
type memWorkspace struct {
	Folder   string // absolute path; empty folders are never stored
	Label    string // filepath.Base(Folder) for display
	Sessions int    // live sessions on this folder; 0 means launch-only
	Launch   bool   // the TUI's own launch-cwd workspace
}

// collectMemoryWorkspaces builds the deduplicated Workspaces list: every live
// session's folder (tallied), plus the launch-cwd workspace when it is not
// already present. The order is stable (sorted by folder) so the cursor never
// jumps between polls.
func (m *Model) collectMemoryWorkspaces() []memWorkspace {
	byFolder := make(map[string]*memWorkspace)
	var order []string
	for _, s := range m.sessions {
		if s.Folder == "" {
			continue
		}
		ws, ok := byFolder[s.Folder]
		if !ok {
			ws = &memWorkspace{Folder: s.Folder, Label: filepath.Base(s.Folder)}
			byFolder[s.Folder] = ws
			order = append(order, s.Folder)
		}
		ws.Sessions++
	}
	if m.dashProjectFolder != "" {
		ws, ok := byFolder[m.dashProjectFolder]
		if !ok {
			ws = &memWorkspace{Folder: m.dashProjectFolder, Label: filepath.Base(m.dashProjectFolder)}
			byFolder[m.dashProjectFolder] = ws
			order = append(order, m.dashProjectFolder)
		}
		ws.Launch = true
	}
	sort.Strings(order)
	out := make([]memWorkspace, 0, len(order))
	for _, f := range order {
		out = append(out, *byFolder[f])
	}
	return out
}

// memDetailRow renders a key/value pair with a 2-space gap. Keys are padded to
// 7 chars so Name/Desc/Size/Created/Updated/Paths all align.
func memDetailRow(k, v string) string {
	const kw = 7
	pad := max(kw-len(k), 0)
	return "  " + KeyStyle.Width(0).Render(k) + strings.Repeat(" ", pad+2) + ValStyle.Render(v)
}

// filteredMemories returns the memories visible under the active filter — a
// case-insensitive substring match on name and description. An empty filter
// returns the full list.
func (m Model) filteredMemories() []memory.Memory {
	if m.memoryFilter == "" {
		return m.memories
	}
	q := strings.ToLower(m.memoryFilter)
	var out []memory.Memory
	for _, mem := range m.memories {
		if strings.Contains(strings.ToLower(mem.Name), q) ||
			strings.Contains(strings.ToLower(mem.Description), q) {
			out = append(out, mem)
		}
	}
	return out
}

// resetMemoryFilterView re-homes cursor, scroll, and body cache after the
// filter (and so the visible list) changed.
func (m *Model) resetMemoryFilterView() {
	m.memoryCursor = 0
	m.leftScroll = 0
	m.rightScroll = 0
	m.memoryBodyCache = ""
	m.memoryBodyCacheName = ""
}

func (m Model) memoryLeftLines() []string {
	lf := m.focusPanel == focusSessions

	var titleStyle lipgloss.Style
	if lf {
		titleStyle = PanelHeaderStyle
	} else {
		titleStyle = PanelHeaderFadedStyle
	}
	mems := m.filteredMemories()
	titleText := fmt.Sprintf(" Memories (%d)", len(m.memories))
	if m.memoryFilter != "" {
		titleText = fmt.Sprintf(" Memories (%d/%d)", len(mems), len(m.memories))
	}

	// The filter status line replaces the header's blank spacer, so the
	// 2-header-line scroll math in scrollToCursor is unaffected.
	filterLine := ""
	if m.memoryFilterActive {
		filterLine = MutedStyle.Render(" ⌕ "+m.memoryFilter) + SelectedStyle.Render("▎")
	} else if m.memoryFilter != "" {
		filterLine = MutedStyle.Render(" ⌕ " + m.memoryFilter)
	}

	lines := []string{titleStyle.Render(titleText), filterLine}
	if len(mems) == 0 {
		msg := " No memories in this workspace."
		if m.memoryFilter != "" {
			msg = " No memories match the filter."
		}
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

	for i, mem := range mems {
		selected := i == m.memoryCursor
		indicator := "∙"
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

func (m Model) memoryRightLines(rw int) []string {
	rf := m.focusPanel == focusDetails
	var headerStyle lipgloss.Style
	if rf {
		headerStyle = PanelHeaderStyle
	} else {
		headerStyle = PanelHeaderFadedStyle
	}

	lines := []string{headerStyle.Render(" Memory Detail"), ""}

	mems := m.filteredMemories()
	if len(mems) == 0 {
		msg := "No memories in this workspace."
		if m.memoryFilter != "" {
			msg = "No memories match the filter."
		}
		lines = append(lines, "  "+MutedStyle.Render(msg))
		return lines
	}

	mem := mems[min(m.memoryCursor, len(mems)-1)]

	const dateFormat = "2006-01-02 15:04"
	lines = append(lines, memDetailRow("Name", mem.Name))
	if mem.Description != "" {
		lines = append(lines, memDetailRow("Desc", mem.Description))
	}
	lines = append(lines, memDetailRow("Size", fmt.Sprintf("%d bytes", mem.SizeBytes)))
	if !mem.CreatedAt.IsZero() {
		lines = append(lines, memDetailRow("Created", mem.CreatedAt.Local().Format(dateFormat)))
	}
	if !mem.ModTime.IsZero() {
		lines = append(lines, memDetailRow("Updated", mem.ModTime.Local().Format(dateFormat)))
	}
	if !mem.UserAuthored() {
		lines = append(lines, memDetailRow("Origin", string(mem.Confidence)+" (machine-written)"))
	}
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

// currentMemoryBody returns the body of the selected memory READ-ONLY from the
// cache. It performs no disk read — that would run on every render frame, since
// the whole render chain operates on a throwaway value copy whose cache writes
// are discarded. populateMemoryBody (a pointer-receiver method on the persisted
// Update flow) is what fills the cache; this only serves it.
func (m Model) currentMemoryBody() string {
	mems := m.filteredMemories()
	if len(mems) == 0 {
		return ""
	}
	name := mems[min(m.memoryCursor, len(mems)-1)].Name
	if m.memoryBodyCacheName == name {
		return m.memoryBodyCache
	}
	return ""
}

// populateMemoryBody reads the selected memory's body from disk into the cache
// when it is stale, off the render path. It runs once per Update on the model
// that is actually returned (see Model.Update), so the disk read happens only
// when the selection or memory list changed and the cache was invalidated —
// never on every render frame. Navigation handlers clear the cache; this fills
// it again for the new selection before the next render reads it.
func (m *Model) populateMemoryBody() {
	if m.currentSection != 2 {
		return
	}
	mems := m.filteredMemories()
	if len(mems) == 0 {
		return
	}
	name := mems[min(m.memoryCursor, len(mems)-1)].Name
	if m.memoryBodyCacheName == name {
		return
	}
	ws := m.memoryFolder
	if ws == "" {
		return
	}
	// Body only — the frontmatter metadata is already shown structured above.
	body, err := memory.ReadBody(ws, name)
	if err != nil {
		body = "(error loading memory: " + err.Error() + ")"
	}
	m.memoryBodyCache = body
	m.memoryBodyCacheName = name
}

func (m *Model) refreshMemories() {
	// Memories render only in the Memory section (currentSection == 2) — skip the
	// directory walk and frontmatter parse otherwise. selectSection triggers an
	// immediate refresh on switching into the section, so this never leaves the
	// pane stale-on-switch.
	if m.currentSection != 2 {
		return
	}
	m.memoryWorkspaces = m.collectMemoryWorkspaces()
	if m.workspaceCursor >= len(m.memoryWorkspaces) {
		m.workspaceCursor = max(len(m.memoryWorkspaces)-1, 0)
	}

	ws := ""
	if len(m.memoryWorkspaces) > 0 {
		ws = m.memoryWorkspaces[m.workspaceCursor].Folder
	}
	if ws != m.memoryFolder {
		m.memoryFolder = ws
		m.memoryFilter = ""
		m.memoryFilterActive = false
		m.resetMemoryFilterView()
	}
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
	if visible := len(m.filteredMemories()); m.memoryCursor >= visible {
		m.memoryCursor = max(visible-1, 0)
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
	// Prime the cache for the now-current selection so the render path (a value
	// copy) can serve the body without a disk read. refreshMemories runs on the
	// persisted model, off the render path.
	m.populateMemoryBody()
}

func (m *Model) selectMemoryAtBodyRow(row int) {
	visible := len(m.filteredMemories())
	if row < 1 || visible == 0 {
		return
	}
	// Each memory occupies 4 rows: name, desc-line1, desc-line2, blank.
	idx := (row - 1) / 4
	if idx < 0 || idx >= visible {
		return
	}
	m.memoryCursor = idx
	m.memoryBodyCache = ""
	m.memoryBodyCacheName = ""
	m.focusPanel = focusSessions
	m.rightScroll = 0
}

// memoryWorkspaceLines renders the Workspaces pane: one line per active
// workspace, the label truncated to fit while the session-count / launch suffix
// stays visible.
func (m Model) memoryWorkspaceLines(wsW int) []string {
	wf := m.focusPanel == focusWorkspaces

	titleStyle := PanelHeaderFadedStyle
	if wf {
		titleStyle = PanelHeaderStyle
	}
	lines := []string{titleStyle.Render(fmt.Sprintf(" Workspaces (%d)", len(m.memoryWorkspaces))), ""}
	if len(m.memoryWorkspaces) == 0 {
		msg := " No active workspaces."
		if wf {
			lines = append(lines, MutedStyle.Render(msg))
		} else {
			lines = append(lines, InactiveStyle.Render(msg))
		}
		return lines
	}

	for i, ws := range m.memoryWorkspaces {
		selected := i == m.workspaceCursor
		indicator := "∙"
		if selected {
			indicator = "❯"
		}
		suffix := ""
		if ws.Sessions > 0 {
			suffix = fmt.Sprintf(" ·%d", ws.Sessions)
		} else if ws.Launch {
			suffix = " ⌂"
		}

		label := ws.Label
		avail := max(wsW-len([]rune(suffix))-4, 4) // " ❯ " prefix + trailing margin
		if labelRunes := []rune(label); len(labelRunes) > avail {
			label = string(labelRunes[:avail-1]) + "…"
		}
		line := " " + indicator + " " + label + suffix

		switch {
		case selected:
			lines = append(lines, SelectedStyle.Render(line))
		case wf:
			lines = append(lines, ItemStyle.Render(line))
		default:
			lines = append(lines, FadedStyle.Render(line))
		}
	}
	return lines
}

// selectWorkspace switches the Workspaces cursor and reloads memories for the
// newly-selected folder (refreshMemories resets the memory cursor and body cache
// when the folder actually changes).
func (m *Model) selectWorkspace(idx int) {
	if idx < 0 || idx >= len(m.memoryWorkspaces) {
		return
	}
	m.workspaceCursor = idx
	m.rightScroll = 0
	m.refreshMemories()
}

func (m *Model) selectWorkspaceAtBodyRow(row int) {
	// Header + blank spacer precede the first workspace row.
	m.focusPanel = focusWorkspaces
	m.selectWorkspace(row - 2)
}
