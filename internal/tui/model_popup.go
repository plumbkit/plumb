package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

func (m Model) popupLeftLines() []string {
	lines := []string{""}
	if len(m.popupCalls) == 0 {
		lines = append(lines, "   "+MutedStyle.Render("No calls recorded yet."))
		return lines
	}
	var currID string
	if len(m.sessions) > 0 {
		currID = m.sessions[m.cursor].ID
	}
	for i, c := range m.popupCalls {
		sc := "·"
		if c.SessionID == currID {
			sc = "•"
		}
		if i == m.popupCallCursor {
			sc = "❯"
		}

		ok := "✓"
		if !c.Success {
			ok = "✗"
		}
		ts := c.CalledAt.Format("01-02 15:04:05")
		dur := fmt.Sprintf("%dms", c.DurationMs)

		// We format it as "  ❯ ✓ 05-19 15:04:05 22ms".
		row := fmt.Sprintf("  %s %s %s %s", sc, ok, ts, dur)
		maxW := max(m.popupLeftWidth-1, 10)
		if lipgloss.Width(row) > maxW {
			row = string([]rune(row)[:maxW-1]) + "…"
		}

		isSelectedRow := i == m.popupCallCursor

		if isSelectedRow && !m.popupRightFocus {
			lines = append(lines, SelectedStyle.Render(row))
		} else if m.popupRightFocus && !isSelectedRow {
			lines = append(lines, TimestampFadedStyle.Render(row))
		} else if !c.Success {
			p1 := fmt.Sprintf("  %s ", sc)
			err := WarnStyle.Render("✗")
			p2 := fmt.Sprintf(" %s %s", ts, dur)
			lines = append(lines, TimestampActiveStyle.Render(p1)+err+TimestampActiveStyle.Render(p2))
		} else {
			lines = append(lines, TimestampActiveStyle.Render(row))
		}
	}
	return lines
}

func (m Model) popupRightAll(rw int) []string {
	lines := []string{""}
	if len(m.popupCalls) == 0 {
		lines = append(lines, "  "+MutedStyle.Render("No calls to show."))
		return lines
	}
	c := m.popupCalls[m.popupCallCursor]
	var currID string
	if len(m.sessions) > 0 {
		currID = m.sessions[m.cursor].ID
	}
	st := OkStyle.Render("✓ success")
	if !c.Success {
		st = WarnStyle.Render("✗ failed")
	}
	sl := MutedStyle.Render("○ historical")
	if c.SessionID == currID {
		sl = OkStyle.Render("● current")
	}
	sID := c.SessionID
	if len(sID) > 12 {
		sID = sID[:12] + "…"
	}
	sessLabel := sID + "  " + sl
	if c.SessionName != "" {
		sessLabel = DetailStyle.Render(c.SessionName) + "  " + sID + "  " + sl
	}
	lines = append(lines,
		detailRow("Tool", DetailStyle.Render(c.Tool)),
		detailRow("Status", st),
		detailRow("Called at", DetailStyle.Render(c.CalledAt.Format("2006-01-02 15:04:05"))),
		detailRow("Session", sessLabel),
		detailRow("Duration", DetailStyle.Render(fmt.Sprintf("%d ms", c.DurationMs))),
		detailRow("Input", DetailStyle.Render(fmt.Sprintf("%d bytes", c.InputBytes))),
		detailRow("Output", DetailStyle.Render(fmt.Sprintf("%d bytes", c.OutputBytes))),
	)

	gutterLine := func(label string, content []string) {
		lines = append(lines, "", "  "+PanelHeaderStyle.Render(label))
		gutterChar := SepStyle.Render("┊")
		for _, cl := range content {
			if lipgloss.Width(cl) > rw-5 {
				cl = string([]rune(cl)[:rw-6]) + "…"
			}
			lines = append(lines, "  "+gutterChar+" "+cl)
		}
	}

	if !c.Success {
		var el []string
		if c.ErrorMsg != "" {
			for _, w := range wrapText(c.ErrorMsg, rw-5) {
				el = append(el, WarnStyle.Render(w))
			}
		} else {
			el = append(el, MutedStyle.Render("(no error message recorded)"))
		}
		gutterLine("Error", el)
	}
	ij, ot := m.currentDetail()
	if ij != "" {
		var al []string
		var pb bytes.Buffer
		if err := json.Indent(&pb, []byte(ij), "", "  "); err == nil {
			for l := range strings.SplitSeq(strings.TrimRight(pb.String(), "\n"), "\n") {
				al = append(al, DetailStyle.Render(l))
			}
		} else {
			al = append(al, DetailStyle.Render(ij))
		}
		gutterLine("Args", al)
	}
	if ot != "" && c.Success {
		var ol []string
		for o := range strings.SplitSeq(strings.TrimRight(ot, "\n"), "\n") {
			ol = append(ol, DetailStyle.Render(o))
		}
		gutterLine("Output", ol)
	}
	return lines
}
