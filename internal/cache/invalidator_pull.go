package cache

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Diagnostic acquisition-source tags (card product-contract). A URI's
// diagnostics may come from pushed textDocument/publishDiagnostics
// notifications, from on-demand textDocument/diagnostic pulls, or from both.
const (
	SourcePush = "lsp-push"
	SourcePull = "lsp-pull"
)

// Pull-diagnostics state (LSP 3.17 textDocument/diagnostic), stored alongside
// the push state and guarded by the SAME diagsMu. Push and pull snapshots for a
// URI are kept independently and exposed as a deduplicated union through the
// existing readers (Diagnostics/AllDiagnostics/Tracked/AllDiagnosticTimes), so
// current consumers see one merged view while Task 5's tool consumes the
// pull-specific methods below.
//
//	pullDiags     — the latest pull snapshot per URI (an empty slice means an
//	                empty full report: a real "no issues" answer, not "unknown").
//	pullResultIDs — the latest non-empty result ID per URI. Absence means "no
//	                known result ID": a subsequent unchanged report can never
//	                match, so the caller must re-pull without a previousResultId.
//	pullTimes     — the last pull update time per URI (folded into
//	                AllDiagnosticTimes for staleness reporting).

// RecordPullFull records a full textDocument/diagnostic report for uri: it
// replaces the URI's pull snapshot outright (an empty diags slice clears it —
// an empty full is a valid "no issues" result) and stores resultID for future
// previousResultId use. An empty resultID records the snapshot but no known
// result ID (the server gave the client nothing to cache against).
//
// It does NOT wake WaitDiagnostics/WaitNextDiagnostics subscribers: those waits
// are push-only by contract (see the Invalidator doc). A pull caller reads the
// recorded snapshot back directly.
//
// The empty-URI boundary check mirrors Handle(): an empty URI is a no-op.
func (inv *Invalidator) RecordPullFull(uri, resultID string, diags []protocol.Diagnostic) {
	if uri == "" {
		return
	}
	inv.diagsMu.Lock()
	defer inv.diagsMu.Unlock()
	inv.recordPullFullLocked(uri, resultID, diags)
}

// recordPullFullLocked is the write body of RecordPullFull. The caller must hold
// diagsMu (write) and must have already rejected an empty uri.
func (inv *Invalidator) recordPullFullLocked(uri, resultID string, diags []protocol.Diagnostic) {
	inv.pullDiags[uri] = cloneDiags(diags)
	inv.pullTimes[uri] = time.Now()
	if resultID != "" {
		inv.pullResultIDs[uri] = resultID
	} else {
		delete(inv.pullResultIDs, uri)
	}
}

// RecordPullUnchanged records an "unchanged" textDocument/diagnostic report:
// when resultID matches the result ID stored for uri, it refreshes the URI's
// pull timestamp and returns true. When the result ID is unknown or does not
// match, it returns false and MUTATES NOTHING — the SAFETY INVARIANT at the
// cache level. A false return tells the caller to re-pull without a
// previousResultId rather than trust a stale-or-absent snapshot.
//
// The empty-URI boundary check mirrors Handle().
func (inv *Invalidator) RecordPullUnchanged(uri, resultID string) (ok bool) {
	if uri == "" {
		return false
	}
	inv.diagsMu.Lock()
	defer inv.diagsMu.Unlock()
	return inv.recordPullUnchangedLocked(uri, resultID)
}

// recordPullUnchangedLocked is the write body of RecordPullUnchanged. The caller
// must hold diagsMu (write) and must have already rejected an empty uri.
func (inv *Invalidator) recordPullUnchangedLocked(uri, resultID string) bool {
	stored, known := inv.pullResultIDs[uri]
	if !known || stored != resultID {
		return false
	}
	inv.pullTimes[uri] = time.Now()
	return true
}

// RecordPullResult ingests a document report and its flat relatedDocuments
// map. It returns deterministic URI lists describing which reports were applied
// and which could not be validated. Unknown or mismatched unchanged result IDs,
// and unrecognised report kinds, mutate nothing and are returned as unresolved.
// If a malformed response reports the same URI more than once, unresolved wins.
func (inv *Invalidator) RecordPullResult(uri string, report protocol.DocumentDiagnosticReport) (applied, unresolved []string) {
	return inv.recordPullResult(uri, report, false, 0)
}

// RecordPullResultAt is RecordPullResult guarded by the pull-state generation
// the caller captured (PullGeneration) BEFORE issuing the pull. If ClearPullState
// has since bumped the generation — a server (re)start or a
// workspace/diagnostic/refresh landed while this pull was in flight — the whole
// report is DROPPED: nothing is applied and every URI it covered is returned as
// unresolved. This closes the window where an in-flight pull completing just
// after a clear re-seeds a stale result ID from the previous server generation
// (a stale ID could later elicit a false "unchanged", and with it a false clean).
// The URIs are returned unresolved rather than silently swallowed so a caller
// never renders a dropped file as clean.
func (inv *Invalidator) RecordPullResultAt(uri string, report protocol.DocumentDiagnosticReport, gen uint64) (applied, unresolved []string) {
	return inv.recordPullResult(uri, report, true, gen)
}

// recordPullResult records a document report and its related documents under a
// single diagsMu write hold, so the generation check and the writes are atomic
// with respect to ClearPullState. When checkGen is set and gen no longer matches
// the current generation, the report is dropped and every URI it covered is
// surfaced as unresolved.
func (inv *Invalidator) recordPullResult(uri string, report protocol.DocumentDiagnosticReport, checkGen bool, gen uint64) (applied, unresolved []string) {
	inv.diagsMu.Lock()
	defer inv.diagsMu.Unlock()

	if checkGen && gen != inv.pullGen {
		return nil, droppedReportURIs(uri, report)
	}

	appliedSet := make(map[string]struct{})
	unresolvedSet := make(map[string]struct{})
	record := func(uri, kind, resultID string, items []protocol.Diagnostic) {
		if uri == "" {
			return
		}
		switch kind {
		case protocol.DiagnosticReportFull:
			inv.recordPullFullLocked(uri, resultID, items)
			appliedSet[uri] = struct{}{}
		case protocol.DiagnosticReportUnchanged:
			if inv.recordPullUnchangedLocked(uri, resultID) {
				appliedSet[uri] = struct{}{}
			} else {
				unresolvedSet[uri] = struct{}{}
			}
		default:
			unresolvedSet[uri] = struct{}{}
		}
	}

	record(uri, report.Kind, report.ResultID, report.Items)
	related := make([]string, 0, len(report.RelatedDocuments))
	for relURI := range report.RelatedDocuments {
		related = append(related, relURI)
	}
	sort.Strings(related)
	for _, relURI := range related {
		rel := report.RelatedDocuments[relURI]
		record(relURI, rel.Kind, rel.ResultID, rel.Items)
	}
	for unresolvedURI := range unresolvedSet {
		delete(appliedSet, unresolvedURI)
	}
	return sortedPullURIs(appliedSet), sortedPullURIs(unresolvedSet)
}

// droppedReportURIs returns every non-empty URI a dropped (stale-generation)
// report covered — the primary plus its related documents — so the caller
// surfaces them as unresolved rather than clean.
func droppedReportURIs(uri string, report protocol.DocumentDiagnosticReport) []string {
	set := make(map[string]struct{}, 1+len(report.RelatedDocuments))
	if uri != "" {
		set[uri] = struct{}{}
	}
	for relURI := range report.RelatedDocuments {
		if relURI != "" {
			set[relURI] = struct{}{}
		}
	}
	return sortedPullURIs(set)
}

// PullGeneration returns the current pull-state generation. A pull caller
// captures it before issuing the request and passes it to RecordPullResultAt so
// a report computed against a since-cleared generation is dropped. The uri
// argument is accepted for interface uniformity with the session routing proxy
// (which routes per URI); the cache's generation is invalidator-global.
func (inv *Invalidator) PullGeneration(string) uint64 {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	return inv.pullGen
}

func sortedPullURIs(set map[string]struct{}) []string {
	uris := make([]string, 0, len(set))
	for uri := range set {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	return uris
}

// PullResultID returns the last result ID recorded for uri via a pull report,
// and whether one is known. Callers pass it as previousResultId on the next
// textDocument/diagnostic request for uri.
func (inv *Invalidator) PullResultID(uri string) (string, bool) {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	id, ok := inv.pullResultIDs[uri]
	return id, ok
}

// AllPullResultIDs returns every (URI, result ID) pair recorded from pull
// reports, sorted by URI for deterministic request payloads. Callers pass the
// slice as previousResultIds on a workspace/diagnostic request so the server
// can answer "unchanged" per document. Returns an empty (non-nil) slice when
// no result IDs are known.
func (inv *Invalidator) AllPullResultIDs() []protocol.PreviousResultID {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	out := make([]protocol.PreviousResultID, 0, len(inv.pullResultIDs))
	for uri, id := range inv.pullResultIDs {
		out = append(out, protocol.PreviousResultID{URI: uri, Value: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out
}

// ClearPullState drops every pull snapshot, result ID, and pull timestamp,
// leaving the push state untouched. It is invoked when a language-server process
// is (re)started (fresh start or wake from hibernation), so the new process
// never matches a previousResultId it did not issue — a stale result ID could
// otherwise elicit a false "unchanged" and, with it, a false clean. Push
// diagnostics are preserved because a fresh server re-publishes them.
func (inv *Invalidator) ClearPullState() {
	inv.diagsMu.Lock()
	defer inv.diagsMu.Unlock()
	inv.pullDiags = make(map[string][]protocol.Diagnostic)
	inv.pullResultIDs = make(map[string]string)
	inv.pullTimes = make(map[string]time.Time)
	inv.pullGen++
}

// DiagnosticSources reports which acquisition channels have contributed
// diagnostics for uri, in the deterministic order push-before-pull. It returns
// an empty slice for a URI neither source has reported on. Task 5 uses it to
// render honest source attribution in the diagnostics tool.
func (inv *Invalidator) DiagnosticSources(uri string) []string {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	var out []string
	if _, ok := inv.diags[uri]; ok {
		out = append(out, SourcePush)
	}
	if _, ok := inv.pullDiags[uri]; ok {
		out = append(out, SourcePull)
	}
	return out
}

// mergedLocked returns the deduplicated union of the push and pull snapshots for
// uri. The caller must hold diagsMu (read or write). It returns nil when neither
// source has an entry, and preserves the exact single-source behaviour (an
// identical copy of the one present snapshot) when only one source has data —
// deduplication runs only when both push and pull data exist for the URI.
func (inv *Invalidator) mergedLocked(uri string) []protocol.Diagnostic {
	push, hasPush := inv.diags[uri]
	pull, hasPull := inv.pullDiags[uri]
	switch {
	case !hasPush && !hasPull:
		return nil
	case hasPush && !hasPull:
		return cloneDiags(push)
	case !hasPush && hasPull:
		return cloneDiags(pull)
	default:
		return mergeDedup(push, pull, inv.diagTimes[uri], inv.pullTimes[uri])
	}
}

// mergeDedup unions push and pull diagnostics for a single URI, deduplicating on
// the exact key URI+range+severity+code+source+message (the URI is constant
// within one call). Output order is deterministic: push snapshot order, then any
// pull-only keys in pull order. For a key present in both snapshots the entry is
// kept once at its first (push) position, and the retained struct is the one
// from the more recently written snapshot (newest-write-wins) — a no-op while
// every Diagnostic field is part of the key, but preserving the documented rule
// against future non-key fields.
func mergeDedup(push, pull []protocol.Diagnostic, pushAt, pullAt time.Time) []protocol.Diagnostic {
	n := len(push) + len(pull)
	result := make([]protocol.Diagnostic, 0, n)
	at := make([]time.Time, 0, n)
	idx := make(map[diagKey]int, n)
	add := func(d protocol.Diagnostic, t time.Time) {
		k := keyOf(d)
		if i, ok := idx[k]; ok {
			if t.After(at[i]) {
				result[i] = d
				at[i] = t
			}
			return
		}
		idx[k] = len(result)
		result = append(result, d)
		at = append(at, t)
	}
	for _, d := range push {
		add(d, pushAt)
	}
	for _, d := range pull {
		add(d, pullAt)
	}
	return result
}

// diagKey is the exact dedup key: range + severity + code + source + message.
// (URI is constant within a single-URI merge, so it is not a field.)
type diagKey struct {
	startLine, startChar uint32
	endLine, endChar     uint32
	severity             protocol.DiagnosticSeverity
	code                 string
	source               string
	message              string
}

func keyOf(d protocol.Diagnostic) diagKey {
	return diagKey{
		startLine: d.Range.Start.Line,
		startChar: d.Range.Start.Character,
		endLine:   d.Range.End.Line,
		endChar:   d.Range.End.Character,
		severity:  d.Severity,
		code:      codeString(d.Code),
		source:    d.Source,
		message:   d.Message,
	}
}

// codeString normalises a Diagnostic.Code (LSP `integer | string`, decoded from
// JSON as string or float64, or set directly as an int in tests) to a stable
// string so codes compare equal regardless of their concrete numeric type.
func codeString(code any) string {
	switch c := code.(type) {
	case nil:
		return ""
	case string:
		return c
	case float64:
		return strconv.FormatFloat(c, 'f', -1, 64)
	case int:
		return strconv.Itoa(c)
	case int64:
		return strconv.FormatInt(c, 10)
	default:
		return fmt.Sprintf("%v", c)
	}
}

// cloneDiags returns a copy of d, preserving nil (no copy) vs empty-non-nil so
// the readers keep their existing nil-for-never-reported contract.
func cloneDiags(d []protocol.Diagnostic) []protocol.Diagnostic {
	if d == nil {
		return nil
	}
	out := make([]protocol.Diagnostic, len(d))
	copy(out, d)
	return out
}
