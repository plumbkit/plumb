package tools

// diagnostics_pull.go — the mode-aware pull half of the diagnostics tool.
// When the connection's resolved diagnostics mode for a URI is "pull" or
// "hybrid" (a negotiated, config-gated outcome — see [lsp.<lang>] diagnostics),
// the tool pulls on demand via textDocument/diagnostic instead of relying on
// pushed publishDiagnostics, records the results in the session cache (result
// IDs included), and serves the merged view. Push mode keeps the historical
// open-and-wait behaviour byte for byte.
//
// SAFETY INVARIANT (card product-contract #4): a pull failure must NEVER turn
// a clean cache into a false "No issues" result. Every error path below either
// falls back to the push machinery only after a genuine downgrade (-32601) or
// degrades explicitly: the error text is surfaced, cached diagnostics are
// shown clearly labelled, and an empty cache is reported as UNVERIFIED.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// Resolved diagnostics-mode vocabulary (the pool's product-contract strings;
// internal/tools cannot import internal/cli without inverting the layering).
const (
	diagModePush   = "push"
	diagModePull   = "pull"
	diagModeHybrid = "hybrid"
)

// maxConcurrentPulls bounds how many textDocument/diagnostic requests a
// multi-URI query keeps in flight at once.
const maxConcurrentPulls = 4

// diagnosticsModer exposes the per-URI resolved diagnostics mode. The session
// routing proxy implements it; older or narrow openers do not, which reads as
// mode "" (push behaviour).
type diagnosticsModer interface {
	DiagnosticsMode(uri string) string
}

// workspacePuller is the optional workspace-pull surface of the opener. uri
// selects the owning server ("" = the connection's primary).
type workspacePuller interface {
	DiagnosticCapabilities(uri string) (interFileDependencies, workspaceDiagnostics bool)
	WorkspaceDiagnostic(ctx context.Context, uri string, params protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error)
}

// pullStateSource is the pull-recording surface of the diagnostics source.
// *cache.Invalidator implements it natively; the session routingInvProxy
// implements it with per-URI routing.
type pullStateSource interface {
	PullResultID(uri string) (string, bool)
	RecordPullUnchanged(uri, resultID string) bool
	RecordPullResult(uri string, report protocol.DocumentDiagnosticReport)
	AllPullResultIDs() []protocol.PreviousResultID
}

// modeFor returns the resolved diagnostics mode for uri, or "" when the opener
// cannot report one (push behaviour).
func (t *Diagnostics) modeFor(uri string) string {
	dm, ok := t.opener.(diagnosticsModer)
	if !ok {
		return ""
	}
	return dm.DiagnosticsMode(uri)
}

// pullModeActive reports whether mode calls for on-demand pulls.
func pullModeActive(mode string) bool {
	return mode == diagModePull || mode == diagModeHybrid
}

// documentPuller is the narrow request surface pullAndRecord needs — both the
// diagnostics tool's opener and a write tool's WriteDeps.Client satisfy it.
type documentPuller interface {
	Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error)
}

// pullAndRecord pulls diagnostics for uri (previousResultId from rec), records
// the outcome through the cache methods, and applies the unknown-result-ID
// rule: an "unchanged" answer that cannot be validated against the stored
// result ID mutates nothing and triggers ONE retry without a previousResultId,
// from which only a full report is trusted. On success it returns the full
// report that was recorded, or nil when a VALIDATED "unchanged" answer means
// the cached snapshot is current. rec may be nil (nothing recorded or
// validated; an "unchanged" answer then takes the retry path).
func pullAndRecord(ctx context.Context, pd documentPuller, rec pullStateSource, uri string) (*protocol.DocumentDiagnosticReport, error) {
	prevID := ""
	if rec != nil {
		prevID, _ = rec.PullResultID(uri)
	}
	rep, err := pd.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
		TextDocument:     protocol.TextDocumentIdentifier{URI: uri},
		PreviousResultID: prevID,
	})
	if err != nil {
		return nil, err
	}
	if rep == nil {
		return nil, fmt.Errorf("language server returned an empty diagnostic response")
	}
	switch rep.Kind {
	case protocol.DiagnosticReportFull:
		if rec != nil {
			rec.RecordPullResult(uri, *rep)
		}
		return rep, nil
	case protocol.DiagnosticReportUnchanged:
		if rec != nil && rec.RecordPullUnchanged(uri, rep.ResultID) {
			return nil, nil // cached snapshot is validated current
		}
		// Unknown or stale result ID (or no cache to validate against): the
		// snapshot cannot be trusted as "unchanged relative to what we hold".
		// Retry once WITHOUT a previousResultId; only a full report is
		// acceptable.
		rep2, err2 := pd.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		})
		if err2 != nil {
			return nil, fmt.Errorf("server answered %q for an unknown result ID and the retry without previousResultId failed: %w", rep.Kind, err2)
		}
		if rep2 == nil || rep2.Kind != protocol.DiagnosticReportFull {
			return nil, fmt.Errorf("server answered %q for an unknown result ID and the retry did not return a full report", rep.Kind)
		}
		if rec != nil {
			rec.RecordPullResult(uri, *rep2)
		}
		return rep2, nil
	default:
		return nil, fmt.Errorf("unrecognised diagnostic report kind %q", rep.Kind)
	}
}

// pullDocument pulls diagnostics for uri via the tool's opener, records the
// outcome, and returns the related-document URIs a full report carried
// (sorted). A nil error with no related URIs covers both a clean full report
// and a validated "unchanged".
func (t *Diagnostics) pullDocument(ctx context.Context, uri string) (related []string, err error) {
	pd, ok := t.opener.(pullDiagnoser)
	if !ok {
		return nil, fmt.Errorf("pull diagnostics unavailable on this connection")
	}
	rec, _ := t.inv.(pullStateSource)
	rep, err := pullAndRecord(ctx, pd, rec, uri)
	if err != nil || rep == nil {
		return nil, err
	}
	related = make([]string, 0, len(rep.RelatedDocuments))
	for relURI := range rep.RelatedDocuments {
		related = append(related, relURI)
	}
	sort.Strings(related)
	return related, nil
}

// singleURIPull is the single-URI path when the resolved mode is pull/hybrid:
// pull immediately, record, and serve the merged (push+pull) cache view for
// the URI and any related documents.
func (t *Diagnostics) singleURIPull(ctx context.Context, uri string) string {
	related, err := t.pullDocument(ctx, uri)
	if err != nil {
		if t.modeFor(uri) == diagModePush {
			// The proxy downgraded the connection (-32601 on a negotiated
			// pull): it is a push connection from here on, so fall back to
			// the push machinery cleanly.
			return t.singleURIPush(ctx, uri)
		}
		// SAFETY INVARIANT: any other pull failure degrades EXPLICITLY — the
		// error is surfaced and the cached state is shown clearly labelled,
		// never re-presented as a fresh "No issues" answer.
		return t.pullDegraded(uri, err)
	}
	byURI := map[string][]protocol.Diagnostic{uri: t.inv.Diagnostics(uri)}
	for _, rel := range related {
		byURI[rel] = t.inv.Diagnostics(rel)
	}
	total := 0
	for _, ds := range byURI {
		total += len(ds)
	}
	if total == 0 {
		return "No issues found — pulled from the language server, file is clean."
	}
	return formatDiagnostics(byURI) + "\n(source=lsp-pull)"
}

// pullDegraded renders the explicit degradation result for a failed pull: the
// error itself, plus the last-known cached diagnostics labelled as possibly
// stale — or, when the cache holds nothing, an unmissable statement that the
// file's state is unverified. It never contains the phrase "No issues".
func (t *Diagnostics) pullDegraded(uri string, err error) string {
	path := paths.URIToPath(uri)
	var sb strings.Builder
	fmt.Fprintf(&sb, "diagnostics pull failed for %s: %v", path, err)
	cached := t.inv.Diagnostics(uri)
	if len(cached) == 0 {
		sb.WriteString("\nNo cached diagnostics are available — this file's state is UNVERIFIED; do not treat it as clean. Retry, or run a build to confirm.")
		return sb.String()
	}
	sb.WriteString("\nShowing the last-known cached diagnostics (possibly stale):\n")
	sb.WriteString(formatDiagnostics(map[string][]protocol.Diagnostic{uri: cached}))
	return sb.String()
}

// multiURI reports on a specific set of files. Pull/hybrid-mode URIs are
// pulled first (bounded concurrency, cancellation-safe); push-mode URIs read
// straight from the cache as before. Output order is deterministic:
// formatDiagnostics sorts by URI and failure notes follow input order.
func (t *Diagnostics) multiURI(ctx context.Context, uris []string) string {
	notes := t.pullAll(ctx, uris)
	merged := make(map[string][]protocol.Diagnostic, len(uris))
	for _, uri := range uris {
		merged[uri] = t.inv.Diagnostics(uri)
	}
	out := formatDiagnostics(merged)
	if len(notes) == 0 {
		return out
	}
	total := 0
	for _, ds := range merged {
		total += len(ds)
	}
	var sb strings.Builder
	if total == 0 {
		// Never leave a bare all-clean claim standing over unverified files.
		fmt.Fprintf(&sb, "No issues found in the %d file(s) that could be checked.", len(uris)-len(notes))
	} else {
		sb.WriteString(out)
	}
	fmt.Fprintf(&sb, "\n⚠ pull failed for %d file(s) — their state is UNVERIFIED (any cached entries above may be stale):", len(notes))
	for _, n := range notes {
		sb.WriteString("\n  " + n)
	}
	return sb.String()
}

// pullAll pulls every pull/hybrid-mode URI in uris with at most
// maxConcurrentPulls in flight. Cancellation-safe: a worker still queued when
// ctx is cancelled records the cancellation instead of pulling. Returns
// failure notes in input order ("" entries filtered).
func (t *Diagnostics) pullAll(ctx context.Context, uris []string) []string {
	if _, ok := t.opener.(pullDiagnoser); !ok {
		return nil
	}
	perURI := make([]string, len(uris))
	sem := make(chan struct{}, maxConcurrentPulls)
	var wg sync.WaitGroup
	for i, uri := range uris {
		if !pullModeActive(t.modeFor(uri)) {
			continue
		}
		wg.Add(1)
		go func(i int, uri string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				perURI[i] = fmt.Sprintf("%s: pull cancelled: %v", paths.URIToPath(uri), ctx.Err())
				return
			}
			if _, err := t.pullDocument(ctx, uri); err != nil {
				if t.modeFor(uri) == diagModePush {
					return // downgraded mid-flight (-32601): the cached read below is the push behaviour
				}
				perURI[i] = fmt.Sprintf("%s: %v", paths.URIToPath(uri), err)
			}
		}(i, uri)
	}
	wg.Wait()
	var notes []string
	for _, n := range perURI {
		if n != "" {
			notes = append(notes, n)
		}
	}
	return notes
}

// allFiles is the no-URI (whole-workspace) path.
func (t *Diagnostics) allFiles(ctx context.Context) string {
	if pullModeActive(t.modeFor("")) {
		return t.allFilesPull(ctx)
	}
	return t.allFilesCached()
}

// allFilesCached is the push-mode no-URI body — byte-identical to the
// historical behaviour.
func (t *Diagnostics) allFilesCached() string {
	if ts, ok := t.inv.(timedDiagnosticsSource); ok {
		return formatDiagnosticsWithTimes(t.inv.AllDiagnostics(), ts.AllDiagnosticTimes())
	}
	return formatDiagnostics(t.inv.AllDiagnostics())
}

// allFilesPull serves the whole-workspace query under pull/hybrid mode. A
// workspace/diagnostic pull is issued ONLY when the owning (primary) server
// advertises workspaceDiagnostics; otherwise the cached view is served with
// one honest note — the tool never scans the repository and never claims
// completeness it cannot back.
func (t *Diagnostics) allFilesPull(ctx context.Context) string {
	wp, ok := t.opener.(workspacePuller)
	rec, okRec := t.inv.(pullStateSource)
	if !ok || !okRec {
		return t.allFilesCachedHonest()
	}
	if _, wsPull := wp.DiagnosticCapabilities(""); !wsPull {
		return t.allFilesCachedHonest()
	}
	rep, err := wp.WorkspaceDiagnostic(ctx, "", protocol.WorkspaceDiagnosticParams{
		PreviousResultIDs: rec.AllPullResultIDs(),
	})
	if err != nil {
		if t.modeFor("") == diagModePush {
			return t.allFilesCached() // downgraded (-32601): push behaviour
		}
		return "workspace diagnostics pull failed: " + err.Error() +
			"\nShowing cached diagnostics — POSSIBLY INCOMPLETE; files not listed are NOT verified.\n" +
			t.allFilesCached()
	}
	for _, item := range rep.Items {
		// Workspace items reuse the per-document report shape; unchanged items
		// with unknown result IDs mutate nothing (cache-level safety).
		rec.RecordPullResult(item.URI, protocol.DocumentDiagnosticReport{
			Kind:     item.Kind,
			ResultID: item.ResultID,
			Items:    item.Items,
		})
	}
	out := t.allFilesCached()
	if strings.HasPrefix(out, "No diagnostics received yet") {
		return "No issues found — a workspace pull returned no diagnostics."
	}
	return out + "\n(refreshed via workspace pull)"
}

// allFilesCachedHonest is the cached view plus the one honest note for a
// pull-mode connection whose server cannot answer workspace-wide queries.
func (t *Diagnostics) allFilesCachedHonest() string {
	return t.allFilesCached() +
		"\nnote: this language server provides diagnostics per file on demand (pull mode) and does not support workspace-wide queries — results above cover only files already analysed or pulled; files not listed are NOT verified. Pass uris to check specific files."
}
