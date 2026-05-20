package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// diagnosticsSource is satisfied by *cache.Invalidator and by the daemon's
// session-level invProxy, which delegates to a shared pool Invalidator.
type diagnosticsSource interface {
	Diagnostics(uri string) []protocol.Diagnostic
	AllDiagnostics() map[string][]protocol.Diagnostic
}

var diagnosticsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uris": {
      "type": "array",
      "items": { "type": "string" },
      "description": "file:// URIs to fetch diagnostics for. Omit or pass [] to return diagnostics for all files that have issues. Pass one URI for a single-file query. Pass multiple URIs to check a specific set of files in one call."
    },
    "uri": {
      "type": "string",
      "description": "Deprecated — use uris instead. Single file:// URI; equivalent to uris: [uri]."
    }
  }
}`)

// Diagnostics exposes LSP diagnostic notifications (errors, warnings, hints)
// that gopls pushes as files are analysed. Results reflect the last snapshot
// received; they may be empty until gopls has finished indexing.
//
// Concurrency: Execute is safe for concurrent use.
type Diagnostics struct {
	inv diagnosticsSource
}

func NewDiagnostics(inv diagnosticsSource) *Diagnostics {
	return &Diagnostics{inv: inv}
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

func (t *Diagnostics) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		URIs []string `json:"uris"`
		URI  string   `json:"uri"` // backward-compat: treated as uris:[uri] when uris is absent
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("diagnostics: invalid arguments: %w", err)
	}

	// Backward-compat: scalar uri field is treated as uris:[uri].
	if len(a.URIs) == 0 && a.URI != "" {
		a.URIs = []string{a.URI}
	}

	switch len(a.URIs) {
	case 0:
		return formatDiagnostics(t.inv.AllDiagnostics()), nil
	case 1:
		return t.singleURI(a.URIs[0]), nil
	default:
		return t.multiURI(a.URIs), nil
	}
}

func (t *Diagnostics) singleURI(uri string) string {
	diags := t.inv.Diagnostics(uri)
	if len(diags) == 0 {
		// Distinguish "analysed and clean" from "never reported on".
		if _, tracked := t.inv.AllDiagnostics()[uri]; !tracked {
			path := strings.TrimPrefix(uri, "file://")
			return fmt.Sprintf("File %s is not yet tracked by the language server. "+
				"Open it in your editor (or run a tool that touches it) so gopls receives a textDocument/didOpen, "+
				"then retry.", path)
		}
		return "No issues found — file is tracked and clean."
	}
	return formatDiagnostics(map[string][]protocol.Diagnostic{uri: diags})
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
