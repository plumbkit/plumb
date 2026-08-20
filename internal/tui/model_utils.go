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

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/stats"
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

func wrapText(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	s = strings.ReplaceAll(s, "\n", " ")
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	// A whitespace-free token (most commonly a client-supplied path embedded
	// in a session HealthMessage, issue #358) has unbounded, caller-controlled
	// length. Left alone it produces a line wider than width, which the
	// caller's box-drawing then cannot pad correctly — lipgloss re-wraps the
	// composed line and the overflow rows arrive with no left border. Breaking
	// every over-wide field into width-wide chunks up front, before packing,
	// guarantees every returned line fits.
	words := make([]string, 0, len(fields))
	for _, f := range fields {
		words = append(words, hardBreak(f, width)...)
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if lipgloss.Width(cur)+1+lipgloss.Width(w) > width {
			lines = append(lines, cur)
			cur = w
		} else {
			cur += " " + w
		}
	}
	return append(lines, cur)
}

// hardBreak splits a single whitespace-free token into width-wide chunks at
// rune boundaries, measuring with lipgloss.Width (display width) rather than
// byte or rune count — this text is unstyled at wrap time, so display width is
// exactly what the caller's box needs. Returns []string{s} unchanged when s
// already fits, so the common case allocates nothing extra. Concatenating the
// returned chunks reproduces s exactly: no rune is dropped or altered.
func hardBreak(s string, width int) []string {
	if lipgloss.Width(s) <= width {
		return []string{s}
	}
	var chunks []string
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > width && b.Len() > 0 {
			chunks = append(chunks, b.String())
			b.Reset()
			w = 0
		}
		b.WriteRune(r)
		w += rw
	}
	if b.Len() > 0 {
		chunks = append(chunks, b.String())
	}
	return chunks
}

func detailRow(k, v string) string { return "  " + KeyStyle.Render(k) + ValStyle.Render(v) }

// contractPath formats p for display in a width-limited column.
// style selects the abbreviation strategy; see config.UIConfig.PathStyle.
func contractPath(p string, maxW int, style string) string {
	p = render.ContractPath(p)
	switch style {
	case "truncate-middle":
		return contractPathTruncateLeft(p, maxW)
	case "full":
		return contractPathFull(p, maxW)
	default: // "compact" and empty/unrecognised
		return contractPathCompact(p, maxW)
	}
}

// contractPathTruncateLeft keeps the rightmost maxW runes, prefixed with "…".
func contractPathTruncateLeft(p string, maxW int) string {
	r := []rune(p)
	if len(r) <= maxW {
		return p
	}
	if maxW <= 1 {
		return "…"
	}
	return "…" + string(r[len(r)-(maxW-1):])
}

// contractPathFull preserves the full path and falls back to "…/<last>" when
// it still overflows, never truncating the final directory component.
func contractPathFull(p string, maxW int) string {
	if len([]rune(p)) <= maxW {
		return p
	}
	base := filepath.Base(p)
	sep := string(filepath.Separator)
	fallback := "…" + sep + base
	if len([]rune(fallback)) <= maxW {
		return fallback
	}
	return contractPathTruncateLeft(base, maxW)
}

// contractPathCompact abbreviates every intermediate directory component to
// its first letter, keeping the final component in full:
//
//	~/Projects/experiments/others/cve-explorer  →  ~/P/e/o/cve-explorer
//
// Falls back to "…/<last>" when the abbreviated form still overflows.
func contractPathCompact(p string, maxW int) string {
	if len([]rune(p)) <= maxW {
		return p
	}
	sep := string(filepath.Separator)
	parts := strings.Split(p, sep)
	// Drop an empty trailing component produced by a trailing separator.
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) < 2 {
		return contractPathTruncateLeft(p, maxW)
	}
	last := parts[len(parts)-1]
	heads := make([]string, len(parts)-1)
	for i, part := range parts[:len(parts)-1] {
		rr := []rune(part)
		if len(rr) <= 1 || part == "~" {
			heads[i] = part
		} else {
			heads[i] = string(rr[:1])
		}
	}
	candidate := strings.Join(heads, sep) + sep + last
	if len([]rune(candidate)) <= maxW {
		return candidate
	}
	fallback := "…" + sep + last
	if len([]rune(fallback)) <= maxW {
		return fallback
	}
	return contractPathTruncateLeft(last, maxW)
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

// sanitisePaste flattens bracketed-paste content for a single-line input:
// newlines and tabs become spaces, other control runes are dropped, and the
// result is trimmed. Unlike typed input, pasted text may carry any printable
// rune (paths and names are not ASCII-only).
func sanitisePaste(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r < 32 || r == 127:
			// drop control runes
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
