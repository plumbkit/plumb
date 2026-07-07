package tools

// post_write_diag.go — post-write diagnostics observation and the compact
// output formatting (size summary, line counts, stale-diagnostic down-ranking)
// shared by edit_file / write_file. Split from file_write_helpers.go to keep
// each file under the ~600-line cap.

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// defaultPostWriteDiagWindow is the fallback window used when WriteDeps.PostWriteDiagWindow
// is zero (i.e. not explicitly configured). Empirically ~150-250ms for gopls on incremental edits.
const defaultPostWriteDiagWindow = 300 * time.Millisecond

// awaitDiagnosticsRefresh waits for the language server to re-publish
// diagnostics for uri after a write, then returns the result. It subscribes to
// the next publishDiagnostics notification and returns the instant the server
// responds — not after a fixed sleep cycle. If the server does not respond in
// time, the most-recent diagnostics for uri are returned (which may predate the
// write).
//
// ceiling semantics: 0 → use defaultPostWriteDiagWindow; negative → disabled,
// return current diagnostics immediately without waiting.
//
// est (nil-safe) adapts the effective wait to how quickly this server actually
// re-publishes: the configured ceiling is an upper bound, and once a typical
// latency is known the wait shrinks toward it so a clean write — one the server
// never re-publishes for — stops paying the full ceiling. Observed publish
// latencies are fed back into est.
//
// The second return value, fresh, reports whether a publish arrived during the
// wait — i.e. the returned diagnostics reflect this write. When false (timeout,
// or the wait was disabled) the diagnostics may predate the write, and callers
// annotate their output accordingly.
func awaitDiagnosticsRefresh(diag postWriteDiagSource, uri string, ceiling time.Duration, est *DiagWaitEstimator) (diags []protocol.Diagnostic, fresh bool) {
	if diag == nil {
		return nil, false
	}
	if ceiling < 0 {
		// Disabled: return the last-known snapshot without waiting — it may
		// predate this write, so it is never fresh.
		return diag.Diagnostics(uri), false
	}
	if ceiling == 0 {
		ceiling = defaultPostWriteDiagWindow
	}
	ctx, cancel := context.WithTimeout(context.Background(), est.window(ceiling))
	defer cancel()
	start := time.Now()
	d, err := diag.WaitNextDiagnostics(ctx, uri)
	if err == nil {
		// A publish landed during the wait, so it reflects this write.
		est.record(time.Since(start))
		return d, true
	}
	return d, false
}

// postWriteDiagSource is the narrow interface write/edit tools need to
// observe post-write diagnostic changes. Satisfied by *cache.Invalidator
// and the daemon's invProxy / routingInvProxy.
type postWriteDiagSource interface {
	Diagnostics(uri string) []protocol.Diagnostic
	WaitNextDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error)
}

// sizeSummary renders a "(N bytes, L lines, C chars)" suffix for write/edit
// responses. chars is the rune count (multibyte-aware): context-window limits
// are character-denominated, so byte-only counts mislead on non-ASCII content
// (from dogfooding feedback).
func sizeSummary(content string) string {
	return fmt.Sprintf("(%d bytes, %d lines, %d chars)", len(content), displayLineCount(content), utf8.RuneCountInString(content))
}

// displayLineCount reports the intuitive number of lines in s: a final newline
// terminates the last line rather than starting a phantom empty one (so
// "a\nb\nc\n" is 3, not 4). Used for the read header and write summaries, where
// an agent expects the count it would see in an editor. Distinct from lineCount
// (deliberately generous) which guards the post-write stale-diagnostic check.
func displayLineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// lineCount reports the number of source lines in s, generously (a trailing
// newline counts as an extra empty line). Used to decide whether a post-write
// diagnostic points beyond the file's current end. Being generous biases the
// out-of-range check toward NOT down-ranking a borderline last-line diagnostic.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// renderDiagGroup appends up to maxPerCategory diagnostics under label, then a
// "…(+N more)" overflow line.
func renderDiagGroup(sb *strings.Builder, label string, diags []protocol.Diagnostic) {
	const maxPerCategory = 3
	for i, x := range diags {
		if i >= maxPerCategory {
			fmt.Fprintf(sb, "\n  %s: …(+%d more)", label, len(diags)-maxPerCategory)
			return
		}
		fmt.Fprintf(sb, "\n  %s L%d: %s", label, x.Range.Start.Line+1, x.Message)
	}
}

// diagIdentity is the line-shift-robust identity of a diagnostic within one
// file: its message plus (stringified) code, deliberately excluding the line.
// Excluding the line lets us recognise a diagnostic the language server carried
// over from before a write even when the edit inserted or removed lines above
// it — the case where an identity keyed on the line would mis-read a shifted
// pre-existing diagnostic as brand new.
type diagIdentity struct {
	message string
	code    string
}

func identityOf(d protocol.Diagnostic) diagIdentity {
	code := ""
	if d.Code != nil {
		code = fmt.Sprint(d.Code)
	}
	return diagIdentity{message: d.Message, code: code}
}

// isReindexLagClass reports whether msg belongs to a gopls diagnostic class that
// an edit most commonly RESOLVES yet the server keeps reporting for a beat while
// it re-indexes — the exact phantom breakage that is the single most-reported
// dogfooding friction. A member of this class that is absent before a write but
// appears after it, on a line the edit touched, is far more likely re-index lag
// than genuine breakage.
func isReindexLagClass(msg string) bool {
	switch {
	case strings.HasPrefix(msg, "undefined:"):
		return true
	case strings.Contains(msg, "imported and not used"):
		return true
	case strings.Contains(msg, "declared and not used"), strings.Contains(msg, "declared but not used"):
		return true
	default:
		return false
	}
}

func lineWithin(line, lo, hi int) bool { return line >= lo && line <= hi }

// changedLineRange reports the 0-based inclusive span of lines in after that
// differ from before, by trimming the common leading and trailing lines. ok is
// false when before is empty or unknown (so the caller must not assume any span
// is "touched") or when the two are identical. A pure deletion collapses to the
// single join-point line. Used only to gate the conservative re-index-lag
// suppression to diagnostics near the edit; the core differencing does not need
// it.
func changedLineRange(before, after string) (lo, hi int, ok bool) {
	if before == "" || before == after {
		return 0, 0, false
	}
	b := strings.Split(before, "\n")
	a := strings.Split(after, "\n")
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	lo = p
	hi = len(a) - 1 - s
	if hi < lo {
		hi = lo // pure deletion: attribute to the join point
	}
	return lo, hi, true
}

// diffFileDiagnostics splits the edited file's post-write diagnostics relative to
// its pre-write set. It is the core of differential post-write diagnostics — the
// fix for the #1 dogfooding complaint that the write response reported STALE
// errors the same edit had already fixed.
//
//   - Every diagnostic carried over from before the write is DROPPED (a
//     pre-existing problem, or a stale copy of one the edit just resolved that
//     the server has not cleared). Matching is by (message, code) and by count,
//     so it is robust to line shifts yet a genuinely ADDITIONAL occurrence of the
//     same message is still reported new.
//   - Among the genuinely new diagnostics, an ERROR of a known re-index-lag class
//     (undefined / imported-and-not-used / declared-and-not-used) that lands on a
//     line the edit touched is separated into likelyStale rather than reported as
//     fresh breakage. This is conservative: nothing is hidden (likelyStale is
//     still surfaced, just clearly labelled), and the class/touched-line gate
//     keeps a genuinely new error the agent introduced — a different message, or
//     one away from the edit — in the fresh group.
//
// Only error and warning severities are considered, matching the rendered set.
func diffFileDiagnostics(pre, post []protocol.Diagnostic, touchedLo, touchedHi int, touchedOK bool) (fresh, likelyStale []protocol.Diagnostic) {
	remaining := make(map[diagIdentity]int, len(pre))
	for _, d := range pre {
		remaining[identityOf(d)]++
	}
	for _, d := range post {
		if d.Severity != protocol.SevError && d.Severity != protocol.SevWarning {
			continue
		}
		id := identityOf(d)
		if remaining[id] > 0 {
			remaining[id]-- // carried over from before the write — not this edit's doing
			continue
		}
		if d.Severity == protocol.SevError && isReindexLagClass(d.Message) &&
			touchedOK && lineWithin(int(d.Range.Start.Line), touchedLo, touchedHi) {
			likelyStale = append(likelyStale, d)
			continue
		}
		fresh = append(fresh, d)
	}
	return fresh, likelyStale
}

// formatDifferentialDiagnostics renders the differential result as a compact
// suffix appended to a write/edit response. Returns "" when nothing new. fresh
// diagnostics past the file's current end (newLineCount) are folded into the
// likely-stale group — a diagnostic pointing past EOF is provably re-index lag
// after a structural edit that shrank the file.
func formatDifferentialDiagnostics(fresh, likelyStale []protocol.Diagnostic, newLineCount int) string {
	var errs, warns, stale []protocol.Diagnostic
	for _, x := range fresh {
		if newLineCount > 0 && int(x.Range.Start.Line) >= newLineCount {
			stale = append(stale, x)
			continue
		}
		if x.Severity == protocol.SevError {
			errs = append(errs, x)
		} else {
			warns = append(warns, x)
		}
	}
	stale = append(stale, likelyStale...)
	if len(errs) == 0 && len(warns) == 0 && len(stale) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\ndiagnostics after write (new since this edit):")
	renderDiagGroup(&sb, "error", errs)
	renderDiagGroup(&sb, "warn", warns)
	renderDiagGroup(&sb, "stale?", stale)
	if len(stale) > 0 {
		sb.WriteString("\n  (stale? = almost certainly re-index lag — a class this edit resolves, or a diagnostic past the file's current end; call diagnostics() to confirm)")
	}
	return sb.String()
}
