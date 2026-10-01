package textdiff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Hunk groups a contiguous changed region with surrounding context lines.
type Hunk struct {
	OldStart, OldCount int
	NewStart, NewCount int
	Lines              []Line
}

// Hunks converts a flat edit script into hunks with ctx lines of context.
func Hunks(script Script, ctx int) []Hunk {
	if len(script) == 0 {
		return nil
	}
	var hunks []Hunk
	i := 0
	oldLine, newLine := 1, 1
	lastEnd := 0

	for i < len(script) {
		for i < len(script) && script[i].Op == Equal {
			oldLine++
			newLine++
			i++
		}
		if i >= len(script) {
			break
		}
		ctxStart := max(lastEnd, i-ctx)
		ctxBack := i - ctxStart
		h := Hunk{
			OldStart: oldLine - ctxBack,
			NewStart: newLine - ctxBack,
		}
		for j := ctxStart; j < i; j++ {
			h.Lines = append(h.Lines, script[j])
			h.OldCount++
			h.NewCount++
		}
		i = collectHunkBody(script, i, ctx, &h, &oldLine, &newLine)
		lastEnd = i
		hunks = append(hunks, h)
	}
	return hunks
}

// collectHunkBody appends lines to h until the trailing common-line run
// reaches ctx length and the next line (if any) is also common.
// Returns the updated index into script.
func collectHunkBody(script Script, i, ctx int, h *Hunk, oldLine, newLine *int) int {
	for i < len(script) {
		dl := script[i]
		h.Lines = append(h.Lines, dl)
		switch dl.Op {
		case Equal:
			*oldLine++
			*newLine++
			h.OldCount++
			h.NewCount++
		case Delete:
			*oldLine++
			h.OldCount++
		case Insert:
			*newLine++
			h.NewCount++
		}
		i++
		if countTrailingCommon(h.Lines) >= ctx && (i >= len(script) || script[i].Op == Equal) {
			break
		}
	}
	return i
}

// countTrailingCommon returns the number of trailing common (space-kind) lines.
func countTrailingCommon(lines []Line) int {
	n := 0
	for j := len(lines) - 1; j >= 0 && lines[j].Op == Equal; j-- {
		n++
	}
	return n
}

// NoEOLMarker follows a diff line whose source line has no terminating newline.
const NoEOLMarker = `\ No newline at end of file`

// FormatHunk renders one hunk: the @@ header, then one prefixed line per entry,
// each NoEOL entry followed by NoEOLMarker.
//
// Convention: OldStart/NewStart are the 1-based index of the first line the
// hunk covers on that side, even when the count is zero (GNU diff writes
// start-1 there). Apply accepts only this convention.
func FormatHunk(h Hunk) []string {
	out := make([]string, 0, 1+len(h.Lines))
	out = append(out, fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.OldStart, h.OldCount, h.NewStart, h.NewCount))
	for _, l := range h.Lines {
		out = append(out, string(l.Op)+l.Text)
		if l.NoEOL {
			out = append(out, NoEOLMarker)
		}
	}
	return out
}

// Render joins the script's hunks (3 lines of context) without a ---/+++
// header. It returns "" when the script has no changes.
func Render(s Script) string {
	var b strings.Builder
	for i, h := range Hunks(s, 3) {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strings.Join(FormatHunk(h), "\n"))
	}
	return b.String()
}

// Unified is Render(ComputeExact(before, after)): an exact, reapplicable diff.
func Unified(before, after string) string {
	if before == after {
		return ""
	}
	return Render(ComputeExact(before, after))
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+),(\d+) \+(\d+),(\d+) @@$`)

// Apply reconstructs after from before and a diff produced by Unified or
// Render. Every context and deleted line is checked against before, so a diff
// applied to the wrong base fails instead of producing plausible garbage.
func Apply(before, unified string) (string, error) {
	if unified == "" {
		return before, nil
	}
	old := splitKeepEOL(before)
	lines := strings.Split(unified, "\n")
	var out []string
	pos := 0
	for i := 0; i < len(lines); {
		m := hunkHeader.FindStringSubmatch(lines[i])
		if m == nil {
			return "", fmt.Errorf("textdiff: diff line %d: expected a hunk header, got %q", i+1, lines[i])
		}
		oldStart, _ := strconv.Atoi(m[1])
		start := oldStart - 1
		if start < pos || start > len(old) {
			return "", fmt.Errorf("textdiff: hunk at diff line %d starts at old line %d, outside %d..%d", i+1, oldStart, pos+1, len(old)+1)
		}
		out = append(out, old[pos:start]...)
		pos = start
		var err error
		if i, pos, out, err = applyHunkBody(lines, i+1, old, pos, out); err != nil {
			return "", err
		}
	}
	out = append(out, old[pos:]...)
	return strings.Join(out, ""), nil
}

func applyHunkBody(lines []string, i int, old []string, pos int, out []string) (int, int, []string, error) {
	for i < len(lines) && !strings.HasPrefix(lines[i], "@@ ") {
		l := lines[i]
		if l == "" {
			return 0, 0, nil, fmt.Errorf("textdiff: diff line %d is empty; every line needs an op prefix", i+1)
		}
		full := l[1:] + "\n"
		if i+1 < len(lines) && lines[i+1] == NoEOLMarker {
			full = l[1:]
			i++
		}
		switch Op(l[0]) {
		case Equal, Delete:
			if pos >= len(old) || old[pos] != full {
				return 0, 0, nil, fmt.Errorf("textdiff: diff line %d does not match the original at line %d", i+1, pos+1)
			}
			if Op(l[0]) == Equal {
				out = append(out, full)
			}
			pos++
		case Insert:
			out = append(out, full)
		default:
			return 0, 0, nil, fmt.Errorf("textdiff: diff line %d has unknown op %q", i+1, l[0])
		}
		i++
	}
	return i, pos, out, nil
}
