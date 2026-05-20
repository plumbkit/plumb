package tui

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/golimpio/plumb/internal/render"
	"github.com/golimpio/plumb/internal/stats"
)

func scrollbarCol(total, visible, offset int, dimmed bool) []string {
	if total <= visible {
		return nil
	}
	ts := max(visible*visible/total, 1)
	mo := max(total-visible, 1)
	tst := offset * (visible - ts) / mo
	col := make([]string, visible)
	thumbStyle := ScrollThumbStyle
	trackStyle := ScrollTrackStyle
	if dimmed {
		thumbStyle = InactiveStyle
		trackStyle = InactiveStyle
	}
	for i := range visible {
		if i >= tst && i < tst+ts {
			col[i] = thumbStyle.Render("┃")
		} else {
			col[i] = trackStyle.Render("│")
		}
	}
	return col
}

func locateCall(calls []stats.RecentCall, key callKey, fallback int) int {
	if !key.zero() {
		for i, c := range calls {
			if c.SessionID == key.sessionID && c.CalledAt.UnixMilli() == key.calledAtMs {
				return i
			}
		}
	}
	if fallback >= len(calls) {
		if len(calls) == 0 {
			return 0
		}
		return len(calls) - 1
	}
	if fallback < 0 {
		return 0
	}
	return fallback
}

func locateTool(stats []stats.ToolStat, toolName string, fallback int) int {
	if toolName != "" {
		for i, t := range stats {
			if t.Tool == toolName {
				return i
			}
		}
	}
	if fallback >= len(stats) {
		if len(stats) == 0 {
			return 0
		}
		return len(stats) - 1
	}
	if fallback < 0 {
		return 0
	}
	return fallback
}

func overlayLogoBottom(line string, width int) string {
	logoBottom := strings.Split(LogoText, "\n")[3]
	logoW := lipgloss.Width(logoBottom)
	if width <= logoW {
		return line
	}
	line = render.PadRight(line, width)

	targetW := width - logoW
	var b strings.Builder
	used := 0
	for _, r := range line {
		rw := lipgloss.Width(string(r))
		if used+rw > targetW {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	if used < targetW {
		b.WriteString(strings.Repeat(" ", targetW-used))
	}
	b.WriteString(logoBottom)
	return b.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

func wrapText(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	s = strings.ReplaceAll(s, "\n", " ")
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
		} else {
			cur += " " + w
		}
	}
	return append(lines, cur)
}

func detailRow(k, v string) string { return "  " + KeyStyle.Render(k) + ValStyle.Render(v) }

func contractPath(p string, max int) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		p = "~" + p[len(h):]
	}
	r := []rune(p)
	if len(r) <= max {
		return p
	}
	if max <= 1 {
		return "…"
	}
	return "…" + string(r[len(r)-(max-1):])
}

func daemonRunning() bool {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	_, err = os.Stat(filepath.Join(base, "plumb", "plumb.sock"))
	return err == nil
}

func copyToClipboard(ij, ot string) tea.Cmd {
	return copyTextToClipboard(formatCallDetailForClipboard(ij, ot))
}

func formatCallDetailForClipboard(ij, ot string) string {
	var buf strings.Builder
	if ij != "" {
		buf.WriteString("=== Args ===\n")
		var pb bytes.Buffer
		if err := json.Indent(&pb, []byte(ij), "", "  "); err == nil {
			buf.WriteString(pb.String())
		} else {
			buf.WriteString(ij)
		}
		buf.WriteString("\n")
	}
	if ot != "" {
		buf.WriteString("=== Output ===\n")
		buf.WriteString(ot)
		buf.WriteString("\n")
	}
	return buf.String()
}

func copyTextToClipboard(txt string) tea.Cmd {
	return func() tea.Msg {
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("pbcopy")
		case "linux":
			if _, err := exec.LookPath("xclip"); err == nil {
				cmd = exec.Command("xclip", "-selection", "clipboard")
			} else {
				cmd = exec.Command("xsel", "--clipboard", "--input")
			}
		}
		if cmd != nil {
			cmd.Stdin = strings.NewReader(txt)
			_ = cmd.Run()
		}
		return nil
	}
}

func spliceOverlay(bg, overlay string, w, h int) string {
	ovLines := strings.Split(overlay, "\n")
	ovH := len(ovLines)
	ovW := 0
	for _, l := range ovLines {
		if lw := lipgloss.Width(l); lw > ovW {
			ovW = lw
		}
	}
	sy, sx := (h-ovH)/2, (w-ovW)/2
	return spliceOverlayAt(bg, overlay, sx, sy)
}

func dimAll(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = InactiveStyle.Render(ansi.Strip(line))
	}
	return strings.Join(lines, "\n")
}

func spliceOverlayAt(bg, overlay string, sx, sy int) string {
	bgLines := strings.Split(bg, "\n")
	ovLines := strings.Split(overlay, "\n")
	ovW := 0
	for _, l := range ovLines {
		if lw := lipgloss.Width(l); lw > ovW {
			ovW = lw
		}
	}
	for i := range ovLines {
		y := sy + i
		if y < 0 || y >= len(bgLines) {
			continue
		}
		bl := bgLines[y]
		ol := ovLines[i]

		// Ensure overlay line is full width
		currOW := lipgloss.Width(ol)
		if currOW < ovW {
			ol += strings.Repeat(" ", ovW-currOW)
		}

		prefix := ansi.Truncate(bl, sx, "")
		suffix := ansi.TruncateLeft(bl, sx+ovW, "")

		bgLines[y] = InactiveStyle.Render(ansi.Strip(prefix)) + ol + InactiveStyle.Render(ansi.Strip(suffix))
	}
	return strings.Join(bgLines, "\n")
}
