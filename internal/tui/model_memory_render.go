package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The Memory section is the only three-pane layout: Workspaces │ Memories ┆
// Detail. Memory is per-workspace, so the Workspaces pane is the primary
// navigation axis — picking a workspace drives which memories the middle pane
// lists and the detail pane shows. The generic two-column renderBodySection
// can't express three columns, so this file provides a dedicated frame
// (mirroring how Dashboard/Logs/Settings each own their renderer).

// memoryColumnWidths splits the body width into the three panes. The Workspaces
// panes default to ~15% / ~25% / ~60% of the body width; the [/] keys override
// the Workspaces or Memories width (memoryWsWidth/memoryMemWidth) and the Detail
// pane absorbs the remainder, so the four separator columns │ ┆ ┆ │ are accounted
// for and the row width equals m.width. Widths are clamped to legible bounds that
// also survive a terminal resize (an absolute override no longer matches a new
// width, so it is re-clamped on read).
func (m Model) memoryColumnWidths() (int, int, int) {
	wsW := m.memoryWsWidth
	if wsW <= 0 {
		wsW = m.width * 15 / 100
	}
	memW := m.memoryMemWidth
	if memW <= 0 {
		memW = m.width * 25 / 100
	}
	wsW = clampWidth(wsW, 12, max(m.width/3, 12))
	memW = clampWidth(memW, 16, max(m.width/2, 16))
	detW := max(m.width-wsW-memW-4, 10)
	return wsW, memW, detW
}

func clampWidth(v, lo, hi int) int { return min(max(v, lo), hi) }

func (m Model) renderMemoryPanels(bodyHeight int, isOverlay bool) string {
	wsW, memW, detW := m.memoryColumnWidths()
	var sb strings.Builder
	sb.WriteString(m.renderMemoryBorder("╭", "╮", "┬", wsW, memW, detW, isOverlay, true) + "\n")
	sb.WriteString(m.renderMemoryBody(wsW, memW, detW, bodyHeight, isOverlay))
	sb.WriteString(m.renderMemoryBorder("╰", "╯", "┴", wsW, memW, detW, isOverlay, false) + "\n")
	return sb.String()
}

// renderMemoryBorder draws the top or bottom frame line with two junctions at
// the inter-pane divider columns. The top line overlays the logo's bottom row.
func (m Model) renderMemoryBorder(left, right, junction string, wsW, memW, detW int, dimmed, top bool) string {
	sepStyle := SepStyle
	if dimmed {
		sepStyle = SepInactiveStyle
	}
	contentW := wsW + memW + detW + 2 // two internal dividers
	filler := []rune(strings.Repeat("─", contentW))
	j := []rune(junction)[0]
	for _, p := range []int{wsW, wsW + 1 + memW} {
		if p >= 0 && p < len(filler) {
			filler[p] = j
		}
	}
	line := left + string(filler) + right
	if top {
		line = overlayLogoBottom(line, m.width)
	}
	return sepStyle.Render(line)
}

// memRowCells holds the pre-resolved text and separator-bar for one body row.
type memRowCells struct {
	ws, mem, det              string
	leftBar, midBar, rightBar string
}

func (m Model) renderMemoryBody(wsW, memW, detW, bodyHeight int, isOverlay bool) string {
	allWs := m.memoryWorkspaceLines(wsW)
	allMem := m.memoryLeftLines()
	allDet := (&m).memoryRightLines(detW)

	maxWsScroll := max(len(allWs)-bodyHeight, 0)
	maxMemScroll := max(len(allMem)-bodyHeight, 0)
	maxDetScroll := max(len(allDet)-bodyHeight, 0)
	if m.scrollBounds != nil {
		m.scrollBounds.maxLeft = maxMemScroll
		m.scrollBounds.maxRight = maxDetScroll
	}
	if m.workspaceScroll > maxWsScroll {
		m.workspaceScroll = maxWsScroll
	}
	if m.leftScroll > maxMemScroll {
		m.leftScroll = maxMemScroll
	}
	if m.rightScroll > maxDetScroll {
		m.rightScroll = maxDetScroll
	}

	wsLines := allWs[m.workspaceScroll:]
	memLines := allMem[m.leftScroll:]
	detLines := allDet[m.rightScroll:]
	wsBar := scrollbarCol(len(allWs), bodyHeight, m.workspaceScroll, isOverlay)
	memBar := scrollbarCol(len(allMem), bodyHeight, m.leftScroll, isOverlay)
	detBar := scrollbarCol(len(allDet), bodyHeight, m.rightScroll, isOverlay)

	var sb strings.Builder
	for i := range bodyHeight {
		ws, wsB := bodyColumn(wsLines, wsBar, i)
		mem, _ := bodyColumn(memLines, memBar, i)
		det, detB := bodyColumn(detLines, detBar, i)
		// The memories pane's scrollbar (when it overflows) sits on the
		// mem|det divider; otherwise the divider is a static ┆.
		midDiv := SepStyle.Render("┆")
		if memBar != nil && i < len(memBar) {
			midDiv = memBar[i]
		}
		cells := memRowCells{ws: ws, mem: mem, det: det, leftBar: wsB, midBar: midDiv, rightBar: detB}
		sb.WriteString(assembleMemoryRow(cells, wsW, memW, detW, isOverlay) + "\n")
	}
	return sb.String()
}

// assembleMemoryRow joins the three column cells and their separators into one
// body line: │ ws ┆ mem ┆ det │, dimming the whole row under an overlay.
func assembleMemoryRow(c memRowCells, wsW, memW, detW int, isOverlay bool) string {
	wsCell := lipgloss.NewStyle().Width(wsW).Render(ansi.Truncate(c.ws, wsW-1, "…") + " ")
	memCell := lipgloss.NewStyle().Width(memW).Render(ansi.Truncate(c.mem, memW-1, "…") + " ")
	detCell := lipgloss.NewStyle().Width(detW).Render(ansi.Truncate(c.det, detW, "…"))
	if isOverlay {
		sep := SepInactiveStyle.Render("┆")
		edge := SepInactiveStyle.Render("│")
		return edge + InactiveStyle.Render(ansi.Strip(wsCell)) + sep +
			InactiveStyle.Render(ansi.Strip(memCell)) + sep +
			InactiveStyle.Render(ansi.Strip(detCell)) + edge
	}
	return c.leftBar + wsCell + SepStyle.Render("┆") + memCell + c.midBar + detCell + c.rightBar
}
