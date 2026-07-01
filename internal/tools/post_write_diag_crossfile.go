package tools

// post_write_diag_crossfile.go — the cross-file half of post-write diagnostics.
// After a write, plumb compares a workspace diagnostics snapshot against a
// baseline captured just before the write and reports NEW errors the edit
// introduced in files OTHER than the one written — the "edit A silently breaks
// B" case the single-file block (post_write_diag.go) cannot see. Two guards keep
// it honest: a baseline diff (so pre-existing errors are never re-flagged as
// fresh breakage) and a per-URI publish-time check (so only files the language
// server actually re-analysed after this write are attributed).

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// crossFileDiagSource is the wider capability the cross-file sweep needs beyond
// postWriteDiagSource: a synchronous whole-workspace diagnostics snapshot plus
// each URI's last publish time. The session routing invalidator implements it;
// narrow test doubles need not, so the sweep degrades to a no-op when a source
// lacks these methods.
type crossFileDiagSource interface {
	AllDiagnostics() map[string][]protocol.Diagnostic
	AllDiagnosticTimes() map[string]time.Time
}

// diagBaseline is a pre-write snapshot of per-URI error state, captured before a
// write mutates the file. Comparing the post-write snapshot against it separates
// errors this edit INTRODUCED from errors that were already present.
type diagBaseline struct {
	at       time.Time                  // captured just before the write
	errCount map[string]int             // error-severity diagnostics per URI
	messages map[string]map[string]bool // error messages per URI (to pick a representative NEW one)
}

// newDiagBaseline snapshots the current workspace error state. Returns nil (a
// cheap no-op the sweep skips) when the source cannot serve a whole-workspace
// snapshot — e.g. a narrow test double.
func newDiagBaseline(src postWriteDiagSource) *diagBaseline {
	cf, ok := src.(crossFileDiagSource)
	if !ok {
		return nil
	}
	all := cf.AllDiagnostics()
	b := &diagBaseline{
		at:       time.Now(),
		errCount: make(map[string]int, len(all)),
		messages: make(map[string]map[string]bool, len(all)),
	}
	for uri, ds := range all {
		for _, d := range ds {
			if d.Severity != protocol.SevError {
				continue
			}
			b.errCount[uri]++
			if b.messages[uri] == nil {
				b.messages[uri] = map[string]bool{}
			}
			b.messages[uri][d.Message] = true
		}
	}
	return b
}

// crossFileBreak is one other file the edit newly broke.
type crossFileBreak struct {
	uri         string
	baseErrs    int
	postErrs    int
	exampleMsg  string // a representative error present now but absent in the baseline
	exampleLine int    // 1-based line of exampleMsg
}

// computeCrossFileDelta returns the files (other than editedURI) whose error
// count rose relative to baseline AND which the language server re-published
// after the baseline was captured. The publish-time guard means a file untouched
// by this write is never attributed; the count/message diff means pre-existing
// errors are never mistaken for fresh breakage. Results are ordered by URI for
// stable output.
func computeCrossFileDelta(base *diagBaseline, post map[string][]protocol.Diagnostic, postTimes map[string]time.Time, editedURI string) []crossFileBreak {
	if base == nil {
		return nil
	}
	var breaks []crossFileBreak
	for uri, ds := range post {
		if uri == editedURI {
			continue // the single-file block already covers the edited file
		}
		if t, ok := postTimes[uri]; !ok || !t.After(base.at) {
			continue // not re-analysed since the write — cannot be its doing
		}
		postErrs, exMsg, exLine := countNewErrors(ds, base.messages[uri])
		if postErrs > base.errCount[uri] {
			breaks = append(breaks, crossFileBreak{
				uri: uri, baseErrs: base.errCount[uri], postErrs: postErrs,
				exampleMsg: exMsg, exampleLine: exLine,
			})
		}
	}
	sort.Slice(breaks, func(i, j int) bool { return breaks[i].uri < breaks[j].uri })
	return breaks
}

// countNewErrors returns the error count in ds and the first error message (with
// its 1-based line) that is absent from baseMsgs — a representative NEW error. A
// nil baseMsgs (the file had no baseline errors) makes every error new.
func countNewErrors(ds []protocol.Diagnostic, baseMsgs map[string]bool) (count int, exampleMsg string, exampleLine int) {
	for _, d := range ds {
		if d.Severity != protocol.SevError {
			continue
		}
		count++
		if exampleMsg == "" && !baseMsgs[d.Message] {
			exampleMsg = d.Message
			exampleLine = int(d.Range.Start.Line) + 1
		}
	}
	return count, exampleMsg, exampleLine
}

// maxCrossFileRows caps how many broken files are listed before an overflow line.
const maxCrossFileRows = 3

// formatCrossFileDiagnostics renders the cross-file heads-up appended after the
// single-file block. Returns "" when nothing new broke. root relativises the
// listed file paths for readability. The wording never over-claims: it states
// the delta, hedges the mid-series case, and points at diagnostics() to confirm.
func formatCrossFileDiagnostics(breaks []crossFileBreak, root string) string {
	if len(breaks) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n⚠ this edit introduced new errors in %d other %s:", len(breaks), plural(len(breaks), "file", "files"))
	for i, b := range breaks {
		if i >= maxCrossFileRows {
			fmt.Fprintf(&sb, "\n  …(+%d more)", len(breaks)-maxCrossFileRows)
			break
		}
		path := relativeToRoot(strings.TrimPrefix(b.uri, "file://"), root)
		delta := b.postErrs - b.baseErrs
		fmt.Fprintf(&sb, "\n  %s: +%d %s (%d → %d)", path, delta, plural(delta, "error", "errors"), b.baseErrs, b.postErrs)
		if b.exampleMsg != "" {
			fmt.Fprintf(&sb, "  e.g. L%d: %s", b.exampleLine, b.exampleMsg)
		}
	}
	sb.WriteString("\nnote: if this is a standalone change (or the last of a series), fix these before moving on; if you're mid-refactor they may clear as you continue. Diagnostics may still be settling — call diagnostics() to confirm.")
	return sb.String()
}
