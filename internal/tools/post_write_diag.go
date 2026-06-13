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

// formatPostWriteDiagnostics renders up to N error/warning diagnostics as a
// compact suffix appended to write/edit_file output. Returns "" if none.
//
// Two staleness guards reduce phantom breakage after a write — the single
// most-reported dogfooding friction:
//
//   - fresh=false: the language server had not re-published within the wait
//     window, so the whole snapshot predates this write. Rendering its
//     (pre-edit) error/warn groups reads as fresh breakage for one beat
//     (e.g. a stale "imported and not used" after the import was already
//     fixed), so we short-circuit to a single "pending" line instead.
//   - newLineCount>0: any error/warning whose line lies beyond the just-written
//     file's current end is provably stale (it points past EOF — the classic
//     case after a structural edit that shrank the file, where gopls still
//     reports old line numbers). These are split into a "stale?" group and never
//     rendered as a hard "error", so an agent does not chase phantom breakage.
func formatPostWriteDiagnostics(d []protocol.Diagnostic, fresh bool, newLineCount int) string {
	if len(d) == 0 {
		return ""
	}
	if !fresh {
		// The snapshot predates this write — every finding in it is pre-edit.
		// Surface a single pending line rather than stale groups that read as
		// fresh breakage (from dogfooding feedback).
		return "\ndiagnostics: pending — LSP not yet re-analysed; call diagnostics() to confirm"
	}
	var errs, warns, stale []protocol.Diagnostic
	for _, x := range d {
		if x.Severity != protocol.SevError && x.Severity != protocol.SevWarning {
			continue
		}
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
	if len(errs) == 0 && len(warns) == 0 && len(stale) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\ndiagnostics after write:")
	renderDiagGroup(&sb, "error", errs)
	renderDiagGroup(&sb, "warn", warns)
	renderDiagGroup(&sb, "stale?", stale)
	if len(stale) > 0 {
		sb.WriteString("\n  (stale? = past the file's current end — almost certainly a pre-edit diagnostic the language server has not yet cleared; rebuild to confirm)")
	}
	// No !fresh branch here: a not-fresh snapshot is short-circuited to the
	// "pending" line at the top of this function, so by this point fresh is always
	// true.
	return sb.String()
}
