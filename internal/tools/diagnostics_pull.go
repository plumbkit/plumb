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
	RecordPullResult(uri string, report protocol.DocumentDiagnosticReport) (applied, unresolved []string)
	AllPullResultIDs() []protocol.PreviousResultID
}

// generationalPullStateSource is the optional generation-guarded record surface.
// The real sources (*cache.Invalidator, the session routingInvProxy) implement
// it; pullAndRecord captures the generation before the request and records under
// it, so a report whose pull state was cleared mid-flight (a server (re)start or
// workspace/diagnostic/refresh) is dropped rather than re-seeding a stale result
// ID. A source without it degrades to the plain RecordPullResult path.
type generationalPullStateSource interface {
	PullGeneration(uri string) uint64
	RecordPullResultAt(uri string, report protocol.DocumentDiagnosticReport, gen uint64) (applied, unresolved []string)
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

type pullRecordResult struct {
	applied    []string
	unresolved []string
}

func (r pullRecordResult) related(primary string) []string {
	related := make([]string, 0, len(r.applied))
	for _, uri := range r.applied {
		if uri != primary {
			related = append(related, uri)
		}
	}
	return related
}

// pullAndRecord pulls diagnostics for uri, records the primary and every related
// report, and returns the cache's validation outcome. An unknown primary
// unchanged result retries exactly once without previousResultId. Unresolved
// related reports do not recurse: they remain explicit and unverified, avoiding
// unbounded server-controlled pull fan-out.
func pullAndRecord(ctx context.Context, pd documentPuller, rec pullStateSource, uri string) (pullRecordResult, error) {
	prevID := ""
	gen, genOK := uint64(0), false
	if rec != nil {
		prevID, _ = rec.PullResultID(uri)
		// Capture the pull-state generation BEFORE the request round-trip, so a
		// clear that lands while the pull is in flight makes the record drop rather
		// than re-seed a stale result ID (the whole operation belongs to one server
		// generation; the retry below reuses the same captured value on purpose).
		if g, ok := rec.(generationalPullStateSource); ok {
			gen, genOK = g.PullGeneration(uri), true
		}
	}
	rep, err := pd.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
		TextDocument:     protocol.TextDocumentIdentifier{URI: uri},
		PreviousResultID: prevID,
	})
	if err != nil {
		return pullRecordResult{}, err
	}
	if rep == nil {
		return pullRecordResult{}, fmt.Errorf("language server returned an empty diagnostic response")
	}
	switch rep.Kind {
	case protocol.DiagnosticReportFull:
		return recordPullReport(rec, uri, rep, gen, genOK), nil
	case protocol.DiagnosticReportUnchanged:
		recorded := recordPullReport(rec, uri, rep, gen, genOK)
		if containsURI(recorded.applied, uri) {
			return recorded, nil
		}
		rep2, err2 := pd.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		})
		if err2 != nil {
			return pullRecordResult{}, fmt.Errorf("server answered %q for an unknown result ID and the retry without previousResultId failed: %w", rep.Kind, err2)
		}
		if rep2 == nil || rep2.Kind != protocol.DiagnosticReportFull {
			return pullRecordResult{}, fmt.Errorf("server answered %q for an unknown result ID and the retry did not return a full report", rep.Kind)
		}
		return recordPullReport(rec, uri, rep2, gen, genOK), nil
	default:
		return pullRecordResult{}, fmt.Errorf("unrecognised diagnostic report kind %q", rep.Kind)
	}
}

func recordPullReport(rec pullStateSource, uri string, rep *protocol.DocumentDiagnosticReport, gen uint64, genOK bool) pullRecordResult {
	var result pullRecordResult
	if rec != nil {
		if g, ok := rec.(generationalPullStateSource); ok && genOK {
			result.applied, result.unresolved = g.RecordPullResultAt(uri, *rep, gen)
		} else {
			result.applied, result.unresolved = rec.RecordPullResult(uri, *rep)
		}
		return result
	}
	if rep.Kind == protocol.DiagnosticReportFull {
		result.applied = []string{uri}
	} else {
		result.unresolved = []string{uri}
	}
	for relatedURI := range rep.RelatedDocuments {
		result.unresolved = append(result.unresolved, relatedURI)
	}
	result.applied = uniqueSortedURIs(result.applied)
	result.unresolved = uniqueSortedURIs(result.unresolved)
	return result
}

func containsURI(uris []string, target string) bool {
	i := sort.SearchStrings(uris, target)
	return i < len(uris) && uris[i] == target
}

// pullDocument pulls diagnostics for uri and returns only related URIs that
// passed this connection's cache/routing boundary, plus any accepted reports
// whose unchanged result ID could not be validated.
func (t *Diagnostics) pullDocument(ctx context.Context, uri string) (related, unresolved []string, err error) {
	pd, ok := t.opener.(pullDiagnoser)
	if !ok {
		return nil, nil, fmt.Errorf("pull diagnostics unavailable on this connection")
	}
	rec, _ := t.inv.(pullStateSource)
	result, err := pullAndRecord(ctx, pd, rec, uri)
	if err != nil {
		return nil, nil, err
	}
	return result.related(uri), result.unresolved, nil
}

// singleURIPull pulls immediately and serves the merged cache view for the
// primary plus every accepted related document.
func (t *Diagnostics) singleURIPull(ctx context.Context, uri string) string {
	related, unresolved, err := t.pullDocument(ctx, uri)
	if err != nil {
		if t.modeFor(uri) == diagModePush {
			return t.singleURIPush(ctx, uri)
		}
		return t.pullDegraded(uri, err)
	}
	byURI := map[string][]protocol.Diagnostic{uri: t.inv.Diagnostics(uri)}
	for _, relatedURI := range related {
		byURI[relatedURI] = t.inv.Diagnostics(relatedURI)
	}
	if len(unresolved) > 0 {
		return formatPullIncomplete(byURI, nil, unresolved)
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

// multiURI reports on requested files plus every accepted related document.
func (t *Diagnostics) multiURI(ctx context.Context, uris []string) string {
	uris = uniqueSortedURIs(uris)
	batch := t.pullAll(ctx, uris)
	allURIs := uniqueSortedURIs(append(append([]string{}, uris...), batch.related...))
	merged := make(map[string][]protocol.Diagnostic, len(allURIs))
	for _, uri := range allURIs {
		merged[uri] = t.inv.Diagnostics(uri)
	}
	if len(batch.notes) == 0 && len(batch.unresolved) == 0 {
		return formatDiagnostics(merged)
	}
	return formatPullIncomplete(merged, batch.notes, batch.unresolved)
}

type pullBatchResult struct {
	related    []string
	unresolved []string
	notes      []string
}

// pullAll pulls every pull/hybrid URI with at most maxConcurrentPulls in flight.
// Per-worker results are combined only after all workers finish, keeping the
// output deterministic and race-free.
func (t *Diagnostics) pullAll(ctx context.Context, uris []string) pullBatchResult {
	// Verification needs at least a file opener; the pull request surface is
	// additionally required for pull/hybrid URIs (checked per-URI below).
	if t.opener == nil {
		return pullBatchResult{}
	}
	_, canPull := t.opener.(pullDiagnoser)
	type perPullResult struct {
		related    []string
		unresolved []string
		note       string
	}
	perURI := make([]perPullResult, len(uris))
	sem := make(chan struct{}, maxConcurrentPulls)
	var wg sync.WaitGroup
	for i, uri := range uris {
		pull := canPull && pullModeActive(t.modeFor(uri))
		// A push-mode URI still needs active verification when its cache is empty
		// and the server has never reported on it — otherwise it renders clean in
		// the merged batch view without ever being checked, unlike the single-URI
		// push path (singleURIPush → openAndWait). A tracked push URI is left to the
		// cache read in multiURI, keeping the common path fast.
		pushVerify := !pull && t.needsPushVerification(uri)
		if !pull && !pushVerify {
			continue
		}
		wg.Add(1)
		go func(i int, uri string, pull bool) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				perURI[i].note = fmt.Sprintf("%s: pull cancelled: %v", paths.URIToPath(uri), ctx.Err())
				return
			}
			perURI[i].related, perURI[i].unresolved, perURI[i].note = t.pullOrVerify(ctx, uri, pull)
		}(i, uri, pull)
	}
	wg.Wait()

	var result pullBatchResult
	for _, per := range perURI {
		result.related = append(result.related, per.related...)
		result.unresolved = append(result.unresolved, per.unresolved...)
		if per.note != "" {
			result.notes = append(result.notes, per.note)
		}
	}
	result.related = uniqueSortedURIs(result.related)
	result.unresolved = uniqueSortedURIs(result.unresolved)
	return result
}

// pullOrVerify runs one batch entry's work (already inside the concurrency
// semaphore): a pull for a pull/hybrid URI, or an open-and-wait verification for
// an untracked push-mode URI. It returns the accepted related and unresolved
// URIs and, when the entry could not be verified, a single UNVERIFIED note.
func (t *Diagnostics) pullOrVerify(ctx context.Context, uri string, pull bool) (related, unresolved []string, note string) {
	path := paths.URIToPath(uri)
	if !pull {
		// Push-mode untracked URI: verify via the same open-and-wait the single-URI
		// path uses. Success populates the cache (read back in multiURI); a failure
		// is surfaced as UNVERIFIED, never silent-clean.
		if verr := t.openAndWaitVerify(ctx, uri); verr != nil {
			note = fmt.Sprintf("%s: %v", path, verr)
		}
		return nil, nil, note
	}
	related, unresolved, err := t.pullDocument(ctx, uri)
	if err == nil {
		return related, unresolved, ""
	}
	if t.modeFor(uri) == diagModePush {
		// The error downgraded this URI to push mid-batch (or a shared-server peer's
		// -32601 did). Re-verify via the same open-and-wait the single-URI path uses
		// instead of only marking it unverified; keep the UNVERIFIED note only when
		// that verification itself cannot complete (safety invariant above).
		if verr := t.openAndWaitVerify(ctx, uri); verr != nil {
			return nil, nil, fmt.Sprintf("%s: pull downgraded to push mid-batch (%v); re-verification failed: %v", path, err, verr)
		}
		return nil, nil, ""
	}
	return nil, nil, fmt.Sprintf("%s: %v", path, err)
}

// needsPushVerification reports whether a push-mode URI must be actively
// verified before the batch can speak to its state: it has no cached diagnostics
// AND the language server has never reported on it. It mirrors the singleURIPush
// "not tracked" gate so a batch treats an untracked push file exactly as the
// single-URI path does — via open-and-wait, never a silent clean.
func (t *Diagnostics) needsPushVerification(uri string) bool {
	return len(t.inv.Diagnostics(uri)) == 0 && !t.inv.Tracked(uri)
}

func uniqueSortedURIs(uris []string) []string {
	set := make(map[string]struct{}, len(uris))
	for _, uri := range uris {
		if uri != "" {
			set[uri] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for uri := range set {
		out = append(out, uri)
	}
	sort.Strings(out)
	return out
}

func formatPullIncomplete(byURI map[string][]protocol.Diagnostic, notes, unresolved []string) string {
	total := 0
	for _, diags := range byURI {
		total += len(diags)
	}
	var sb strings.Builder
	if total == 0 {
		sb.WriteString("No verified diagnostic findings were returned for the files that could be checked.")
	} else {
		sb.WriteString(formatDiagnostics(byURI))
	}
	if len(notes) > 0 {
		fmt.Fprintf(&sb, "\n⚠ pull failed for %d file(s) — their state is UNVERIFIED (any cached entries above may be stale):", len(notes))
		for _, note := range notes {
			sb.WriteString("\n  " + note)
		}
	}
	if len(unresolved) > 0 {
		sb.WriteString("\n" + unverifiedPullNote(unresolved))
	}
	return sb.String()
}

func unverifiedPullNote(uris []string) string {
	uris = uniqueSortedURIs(uris)
	var sb strings.Builder
	fmt.Fprintf(&sb, "⚠ %d diagnostic report(s) could not be validated — state unverified (UNVERIFIED); do not treat these files as clean:", len(uris))
	for _, uri := range uris {
		sb.WriteString("\n  " + paths.URIToPath(uri))
	}
	return sb.String()
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
			return t.allFilesCached()
		}
		return "workspace diagnostics pull failed: " + err.Error() +
			"\nShowing cached diagnostics — POSSIBLY INCOMPLETE; files not listed are NOT verified.\n" +
			t.allFilesCached()
	}
	if rep == nil {
		return "workspace diagnostics pull failed: language server returned an empty response" +
			"\nShowing cached diagnostics — POSSIBLY INCOMPLETE; files not listed are NOT verified.\n" +
			t.allFilesCached()
	}
	var unresolved []string
	for _, item := range rep.Items {
		_, itemUnresolved := rec.RecordPullResult(item.URI, protocol.DocumentDiagnosticReport{
			Kind:     item.Kind,
			ResultID: item.ResultID,
			Items:    item.Items,
		})
		unresolved = append(unresolved, itemUnresolved...)
	}
	unresolved = uniqueSortedURIs(unresolved)
	if len(unresolved) > 0 {
		return formatPullIncomplete(t.inv.AllDiagnostics(), nil, unresolved) +
			"\n(workspace pull incomplete)"
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
