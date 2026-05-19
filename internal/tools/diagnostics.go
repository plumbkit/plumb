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
    "uri": {
      "type": "string",
      "description": "file:// URI to fetch diagnostics for. Omit to return all files that have issues."
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

func (t *Diagnostics) Name() string             { return "diagnostics" }
func (t *Diagnostics) InputSchema() json.RawMessage { return diagnosticsSchema }
func (t *Diagnostics) Description() string {
	return "Return LSP errors, warnings, and hints for a file or for all files in the workspace. " +
		"Results are pushed by the language server as it analyses code; they may be empty " +
		"if the server has not yet sent any diagnostics."
}

func (t *Diagnostics) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("diagnostics: invalid arguments: %w", err)
	}

	if a.URI != "" {
		diags := t.inv.Diagnostics(a.URI)
		if len(diags) == 0 {
			// Distinguish "analysed and clean" from "never reported on".
			if _, tracked := t.inv.AllDiagnostics()[a.URI]; !tracked {
				path := strings.TrimPrefix(a.URI, "file://")
				return fmt.Sprintf("File %s is not yet tracked by the language server. "+
					"Open it in your editor (or run a tool that touches it) so gopls receives a textDocument/didOpen, "+
					"then retry.", path), nil
			}
			return "No issues found — file is tracked and clean.", nil
		}
		return formatDiagnostics(map[string][]protocol.Diagnostic{a.URI: diags}), nil
	}

	all := t.inv.AllDiagnostics()
	return formatDiagnostics(all), nil
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
