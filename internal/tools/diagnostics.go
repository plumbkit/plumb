package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// diagnosticsSource is satisfied by *cache.Invalidator and by the daemon's
// session-level invProxy, which delegates to a shared pool Invalidator.
type diagnosticsSource interface {
	Diagnostics(uri string) []protocol.Diagnostic
	AllDiagnostics() map[string][]protocol.Diagnostic
	Tracked(uri string) bool
}

// timedDiagnosticsSource extends diagnosticsSource with per-URI timestamps so
// the tool can warn about entries that may be stale relative to the file mtime.
type timedDiagnosticsSource interface {
	diagnosticsSource
	AllDiagnosticTimes() map[string]time.Time
}

// waitableDiagnosticsSource extends diagnosticsSource with on-demand analysis.
type waitableDiagnosticsSource interface {
	diagnosticsSource
	WaitDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error)
}

// fileOpener triggers language-server analysis for a single file.
type fileOpener interface {
	DidOpen(ctx context.Context, params protocol.DidOpenTextDocumentParams) error
	DidClose(ctx context.Context, params protocol.DidCloseTextDocumentParams) error
}

// pullDiagnoser is the optional LSP 3.17 pull-diagnostics capability
// (textDocument/diagnostic). The diagnostics tool type-asserts its opener to it
// and, when the server advertised pull support, requests diagnostics directly
// for a file the push stream never reported on — the model zls and
// typescript-language-server use instead of pushing publishDiagnostics. Adapters
// that only push (gopls, pyright) do not implement it, so the pull path stays
// dormant for them.
//
// NOTE: this consumption path is routed live through the session proxy
// (routingProxy implements this interface by delegating to the per-file
// adapter), so it fires for any installed server that advertises pull support.
// The pull *client* capability is still deliberately NOT advertised in
// DefaultClientCapabilities: advertising it risks a dual-mode server (gopls)
// switching to pull-only and ceasing to push publishDiagnostics, which would
// regress the validated push path. With it un-advertised, no currently-installed
// server (gopls, zls) advertises diagnosticProvider, so the live path is dormant
// in practice until a pull-only server is in use — but it is no longer dormant by
// construction. See the diagnostics design notes.
type pullDiagnoser interface {
	SupportsPullDiagnostics() bool
	Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error)
}

var diagnosticsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uris": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Absolute paths, file:// URIs, or workspace-relative paths to fetch diagnostics for. Omit or pass [] to return diagnostics for all files that have issues. Pass one for a single-file query. Pass multiple to check a specific set of files in one call."
    },
    "uri": {
      "type": "string",
      "description": "Deprecated — use uris instead. Single file:// URI; equivalent to uris: [uri]."
    }
  },
  "additionalProperties": false
}`)

// Diagnostics exposes LSP diagnostic notifications (errors, warnings, hints)
// that gopls pushes as files are analysed. Results reflect the last snapshot
// received; they may be empty until gopls has finished indexing.
//
// When opener is non-nil and a requested URI is not yet tracked, the tool
// sends textDocument/didOpen to trigger analysis and waits up to 10 s for
// gopls to push publishDiagnostics before returning.
//
// Concurrency: Execute is safe for concurrent use.
type Diagnostics struct {
	inv    waitableDiagnosticsSource
	opener fileOpener // nil when no LSP client is available
	guard  BoundaryGuard
	ws     WorkspaceFn // may be nil; anchors workspace-relative uris to the pinned root
}

func NewDiagnostics(inv waitableDiagnosticsSource) *Diagnostics {
	return &Diagnostics{inv: inv}
}

func NewDiagnosticsWithOpener(inv waitableDiagnosticsSource, opener fileOpener) *Diagnostics {
	return &Diagnostics{inv: inv, opener: opener}
}

func (t *Diagnostics) WithBoundary(guard BoundaryGuard) *Diagnostics {
	t.guard = guard
	return t
}

// WithWorkspace wires the pinned-workspace accessor so relative uri/uris
// resolve against the workspace root rather than the daemon's working
// directory. Nil-safe.
func (t *Diagnostics) WithWorkspace(ws WorkspaceFn) *Diagnostics {
	t.ws = ws
	return t
}

func (t *Diagnostics) Name() string                 { return "diagnostics" }
func (t *Diagnostics) InputSchema() json.RawMessage { return diagnosticsSchema }
func (t *Diagnostics) Description() string {
	return "Return LSP errors, warnings, and hints for one file, several files, or the whole workspace. " +
		"Pass uris (a list of file:// URIs) to check specific files — omit or pass [] to query all files. " +
		"A single call with multiple URIs replaces multiple single-file calls. " +
		"Results are pushed by the language server as it analyses code; they may be empty " +
		"if the server has not yet sent any diagnostics."
}

func (t *Diagnostics) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		URIs []string `json:"uris"`
		URI  string   `json:"uri"` // backward-compat: treated as uris:[uri] when uris is absent
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("diagnostics: invalid arguments: %w", err)
	}
	a.URI = toFileURIAnchored(a.URI, t.ws)
	for i := range a.URIs {
		a.URIs[i] = toFileURIAnchored(a.URIs[i], t.ws)
	}

	// Backward-compat: scalar uri field is treated as uris:[uri].
	if len(a.URIs) == 0 && a.URI != "" {
		a.URIs = []string{a.URI}
	}
	for _, uri := range a.URIs {
		if err := t.guard.check(strings.TrimPrefix(uri, "file://")); err != nil {
			return "", fmt.Errorf("diagnostics: %w", err)
		}
	}

	switch len(a.URIs) {
	case 0:
		if ts, ok := t.inv.(timedDiagnosticsSource); ok {
			return formatDiagnosticsWithTimes(t.inv.AllDiagnostics(), ts.AllDiagnosticTimes()), nil
		}
		return formatDiagnostics(t.inv.AllDiagnostics()), nil
	case 1:
		return t.singleURI(ctx, a.URIs[0]), nil
	default:
		return t.multiURI(a.URIs), nil
	}
}

func (t *Diagnostics) singleURI(ctx context.Context, uri string) string {
	diags := t.inv.Diagnostics(uri)
	if len(diags) == 0 {
		// Distinguish "analysed and clean" from "never reported on".
		if !t.inv.Tracked(uri) {
			// A pull-only server (zls / typescript-language-server) never pushes,
			// so the push cache is empty: ask it directly before opening + waiting.
			if pulled, ok := t.tryPull(ctx, uri); ok {
				return pulled
			}
			if t.opener != nil {
				return t.openAndWait(ctx, uri)
			}
			path := strings.TrimPrefix(uri, "file://")
			return fmt.Sprintf("File %s is not yet tracked by the language server. "+
				"No LSP client is available to trigger analysis.", path)
		}
		return "No issues found — file is tracked and clean."
	}
	return formatDiagnostics(map[string][]protocol.Diagnostic{uri: diags})
}

// tryPull requests diagnostics for uri via the LSP 3.17 pull model when the
// opener implements pullDiagnoser and the server advertised pull support. It
// folds in any related-document diagnostics the server returns. Returns
// ok=false when pull is unavailable or errors, so the caller falls back to the
// push (open-and-wait) path.
func (t *Diagnostics) tryPull(ctx context.Context, uri string) (string, bool) {
	if t.opener == nil {
		return "", false
	}
	pd, ok := t.opener.(pullDiagnoser)
	if !ok || !pd.SupportsPullDiagnostics() {
		return "", false
	}
	rep, err := pd.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil || rep == nil {
		return "", false
	}
	byURI := map[string][]protocol.Diagnostic{}
	if len(rep.Items) > 0 {
		byURI[uri] = rep.Items
	}
	for relURI, relRep := range rep.RelatedDocuments {
		if len(relRep.Items) > 0 {
			byURI[relURI] = relRep.Items
		}
	}
	if len(byURI) == 0 {
		return "No issues found — pulled from the language server, file is clean.", true
	}
	return formatDiagnostics(byURI) + "\n(source=lsp-pull)", true
}

// openAndWait sends textDocument/didOpen for uri, waits up to 10 s for gopls
// to push publishDiagnostics, then sends didClose and returns the result.
func (t *Diagnostics) openAndWait(ctx context.Context, uri string) string {
	path := strings.TrimPrefix(uri, "file://")
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("cannot read %s: %v", path, err)
	}
	if openErr := t.opener.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri,
			LanguageID: languageIDFromURI(uri),
			Version:    1,
			Text:       string(content),
		},
	}); openErr != nil {
		return fmt.Sprintf("DidOpen failed for %s: %v", path, openErr)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	diags, waitErr := t.inv.WaitDiagnostics(waitCtx, uri)
	_ = t.opener.DidClose(ctx, protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if waitErr != nil {
		return fmt.Sprintf("timed out waiting for diagnostics for %s (gopls may still be indexing)", path)
	}
	return formatDiagnostics(map[string][]protocol.Diagnostic{uri: diags})
}

func languageIDFromURI(uri string) string {
	lower := strings.ToLower(uri)
	switch {
	case strings.HasSuffix(lower, ".go"):
		return "go"
	case strings.HasSuffix(lower, ".py"):
		return "python"
	case strings.HasSuffix(lower, ".java"):
		return "java"
	default:
		return "plaintext"
	}
}

func (t *Diagnostics) multiURI(uris []string) string {
	merged := make(map[string][]protocol.Diagnostic, len(uris))
	for _, uri := range uris {
		merged[uri] = t.inv.Diagnostics(uri)
	}
	return formatDiagnostics(merged)
}

// FormatDiagnostics renders a URI→diagnostics map as a human-readable string.
// It is exported so the daemon's control-socket handler can produce live output
// without going through the MCP tool layer.
func FormatDiagnostics(byURI map[string][]protocol.Diagnostic) string {
	return formatDiagnostics(byURI)
}

// formatDiagnosticsWithTimes renders diagnostics with a staleness warning for
// files that have been modified on disk after their last publishDiagnostics.
func formatDiagnosticsWithTimes(byURI map[string][]protocol.Diagnostic, times map[string]time.Time) string {
	if len(byURI) == 0 {
		return "No diagnostics received yet. The language server may still be indexing."
	}

	uris := make([]string, 0, len(byURI))
	for uri := range byURI {
		uris = append(uris, uri)
	}
	sort.Strings(uris)

	total := 0
	for _, uri := range uris {
		total += len(byURI[uri])
	}
	if total == 0 {
		return "No issues found — all tracked files are clean."
	}

	// Identify files with errors whose mtime is newer than the diagnostic timestamp.
	staleURIs := make(map[string]bool)
	for uri, diagTime := range times {
		if len(byURI[uri]) == 0 {
			continue
		}
		path := strings.TrimPrefix(uri, "file://")
		fi, err := os.Stat(path)
		if err == nil && fi.ModTime().After(diagTime) {
			staleURIs[uri] = true
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d issue(s) across %d file(s)\n", total, len(byURI))
	if len(staleURIs) > 0 {
		fmt.Fprintf(&sb, "Note: %d file(s) modified since last analysis — diagnostics may not reflect current state.\n", len(staleURIs))
	}

	for _, uri := range uris {
		diags := byURI[uri]
		if len(diags) == 0 {
			continue
		}
		path := strings.TrimPrefix(uri, "file://")
		if staleURIs[uri] {
			fmt.Fprintf(&sb, "\n%s  (file modified after last analysis — may be stale)\n", path)
		} else {
			fmt.Fprintf(&sb, "\n%s\n", path)
		}
		for _, d := range diags {
			sev := severityLabel(d.Severity)
			line := d.Range.Start.Line + 1
			col := d.Range.Start.Character + 1
			fmt.Fprintf(&sb, "  %s  %d:%d  %s\n", sev, line, col, d.Message)
		}
	}
	return sb.String()
}

func formatDiagnostics(byURI map[string][]protocol.Diagnostic) string {
	if len(byURI) == 0 {
		return "No diagnostics received yet. The language server may still be indexing."
	}

	// Sort URIs for deterministic output.
	uris := make([]string, 0, len(byURI))
	for uri := range byURI {
		uris = append(uris, uri)
	}
	sort.Strings(uris)

	var sb strings.Builder
	total := 0
	for _, uri := range uris {
		total += len(byURI[uri])
	}

	if total == 0 {
		return "No issues found — all tracked files are clean."
	}

	fmt.Fprintf(&sb, "%d issue(s) across %d file(s)\n", total, len(byURI))

	for _, uri := range uris {
		diags := byURI[uri]
		if len(diags) == 0 {
			continue
		}
		path := strings.TrimPrefix(uri, "file://")
		fmt.Fprintf(&sb, "\n%s\n", path)
		for _, d := range diags {
			sev := severityLabel(d.Severity)
			line := d.Range.Start.Line + 1
			col := d.Range.Start.Character + 1
			fmt.Fprintf(&sb, "  %s  %d:%d  %s\n", sev, line, col, d.Message)
		}
	}
	return sb.String()
}

func severityLabel(s protocol.DiagnosticSeverity) string {
	switch s {
	case protocol.SevError:
		return "ERROR  "
	case protocol.SevWarning:
		return "WARN   "
	case protocol.SevInformation:
		return "INFO   "
	case protocol.SevHint:
		return "HINT   "
	default:
		return "UNKNOWN"
	}
}
