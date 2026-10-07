// Package textfmt provides the stdlib-only text primitives shared across every
// layer: pluralisation, truncation, byte-size formatting, and making untrusted
// text safe to print to a terminal.
//
// It exists as its own Foundation package rather than living in internal/render
// because render imports lipgloss. These helpers are wanted by internal/memory
// (Domain), internal/topology (Intelligence) and internal/tools (Application) —
// none of which should drag a terminal rendering library into their dependency
// graph to format a byte count. The layering rule permits importing render from
// any of them; dependency weight is the reason not to.
//
// Nothing here may import another plumb package. Everything is a pure function,
// safe for concurrent use.
package textfmt

import (
	"fmt"
	"unicode/utf8"
)

// ellipsis is the single-rune trim marker used by Ellipsis and ClampBytes.
// One rune, three bytes — the distinction matters, which is exactly why the
// rune-budget and byte-budget helpers below are separate functions.
const ellipsis = "…"

// Countable is the set of integer kinds the counts in plumb's output actually
// come in: plain int from slice lengths, int64 from database aggregates.
type Countable interface {
	~int | ~int32 | ~int64
}

// ByteCount is the set of integer kinds byte totals arrive as: int64 from file
// sizes and database aggregates, uint64 from runtime.MemStats.
type ByteCount interface {
	~int | ~int64 | ~uint64
}

// Plural picks one or many by n. It takes whole words rather than a suffix so
// irregular forms ("entry"/"entries", "is"/"are") work without the caller
// splitting a stem, and it is generic so an int64 row count and an int slice
// length can share one helper — the int/int64 split is what kept the TUI and
// tool copies of this apart.
func Plural[T Countable](n T, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Ellipsis truncates s to at most n runes TOTAL, including the trailing
// ellipsis, so the result always fits a column n cells wide. Returns s
// unchanged when it already fits, and "" for a non-positive budget — a
// zero-width column gets nothing rather than a stray glyph.
//
// The budget is counted in runes, never bytes: slicing a string by byte offset
// can land inside a UTF-8 sequence and emit a replacement character.
// For a budget that is genuinely in bytes (a config knob named *_bytes, say),
// use ClampBytes instead.
func Ellipsis(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return ellipsis
	}
	return string(r[:n-1]) + ellipsis
}

// TruncateBytes returns the longest prefix of s that is at most n bytes and
// ends on a rune boundary. It appends nothing — use ClampBytes when the result
// should be marked as trimmed.
func TruncateBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n-- // back up off a continuation byte to the rune boundary
	}
	return s[:n]
}

// ClampBytes truncates s so the result is at most budget BYTES including the
// trailing ellipsis, cut on a rune boundary. A no-op when s already fits or
// budget is non-positive.
//
// Byte-budgeted rather than rune-budgeted on purpose: the config knobs that
// drive it are named *_bytes, so a CJK or emoji summary must be measured the
// way the knob promises, not by rune count.
func ClampBytes(s string, budget int) string {
	clamped, _ := ClampBytesKept(s, budget)
	return clamped
}

// ClampBytesKept is ClampBytes plus the number of bytes OF S that survived the
// cut. That is not len(clamped): the ellipsis spends three bytes of the budget
// without carrying any of s, and backing the cut up to a rune boundary can
// drop three more, so the two can differ by six.
//
// Any caller reporting "N of M bytes arrived" needs kept, not len(clamped) —
// otherwise it overstates what was delivered and points a resend at an offset
// that skips content neither side knows was dropped.
func ClampBytesKept(s string, budget int) (clamped string, kept int) {
	if budget <= 0 || len(s) <= budget {
		return s, len(s)
	}
	if budget <= len(ellipsis) {
		clamped = TruncateBytes(s, budget) // no room for content plus the marker
		return clamped, len(clamped)
	}
	prefix := TruncateBytes(s, budget-len(ellipsis))
	return prefix + ellipsis, len(prefix)
}

// HumanBytes formats a byte count for one-line CLI, TUI and tool output.
//
// Units are binary and labelled as such (KiB/MiB/GiB). The distinction is not
// pedantry here: every copy this replaces divided by 1024, and two of them
// labelled the result "KB" — stating a different number than they computed.
func HumanBytes[T ByteCount](b T) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// HumanBytesCompact is HumanBytes with the fractional digit dropped below GiB
// ("512 KiB", not "512.0 KiB"). Used by the memory dashboards and `plumb debug`
// rows, where several byte columns sit side by side and the extra two cells per
// column cost more than the precision is worth.
func HumanBytesCompact[T ByteCount](b T) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// TerminalSafe makes untrusted text safe to print to a terminal. Every C0
// control except newline and tab, plus DEL, is shown in caret notation (ESC as
// "^[", BEL as "^G", CR as "^M"), so no escape sequence can run: a file or path
// with a CSI clear screen or an OSC 52 clipboard write is displayed, not
// executed, and a lone CR cannot rewrite the line. C1 controls (U+0080–U+009F,
// which some terminals also honour) become U+FFFD, and so does every byte that
// is not valid UTF-8: a raw 0x9B is CSI to a terminal reading 8-bit controls,
// though a Go range loop sees only U+FFFD, so the output must not keep it. Text
// without any of these is returned unchanged, unallocated. It is for display
// only: output meant for another program (a patch, JSON) should not be
// scrubbed.
func TerminalSafe(s string) string {
	clean := true
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if needsTerminalScrub(r, size) {
			clean = false
			break
		}
		i += size
	}
	if clean {
		return s
	}
	b := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case !needsTerminalScrub(r, size):
			b = utf8.AppendRune(b, r)
		case r == 0x7f:
			b = append(b, '^', '?')
		case r < 0x20:
			b = utf8.AppendRune(append(b, '^'), r+'@')
		default: // C1, or a byte that is not valid UTF-8
			b = utf8.AppendRune(b, utf8.RuneError)
		}
	}
	return string(b)
}

// needsTerminalScrub reports whether the rune decoded from size bytes must be
// rewritten. An invalid byte decodes as RuneError with size 1; a literal
// U+FFFD in the text (size 3) is fine as it is.
func needsTerminalScrub(r rune, size int) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
		(r == utf8.RuneError && size == 1)
}
