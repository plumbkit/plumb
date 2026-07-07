package cli

import (
	"context"
	"fmt"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// ─── Pull diagnostics (textDocument/diagnostic) ────────────────────────────
//
// routingProxy is the per-connection LSP handle the diagnostics tool is
// constructed with. The tool type-asserts that handle to its pullDiagnoser
// interface (SupportsPullDiagnostics + Diagnostic) and, for an untracked file a
// pull-only server never pushed on, requests diagnostics directly. The proxy
// satisfies that interface structurally by delegating to the per-file adapter:
// before these methods existed the assertion failed at runtime and the pull
// path was dormant live. The path is purely additive — the tool only reaches it
// when the push cache is empty for an untracked URI and the routed adapter both
// implements pull and reports the server advertised it.

// pullCapableClient is the optional pull-diagnostics surface an underlying
// adapter (zls, typescript-language-server) may expose. Resolved structurally
// from the routed lsp.Client.
type pullCapableClient interface {
	SupportsPullDiagnostics() bool
	Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error)
}

// SupportsPullDiagnostics reports whether the connection's primary adapter
// supports the LSP 3.17 pull model. URI-less by nature (the diagnostics tool
// calls it before it has routed a specific file), so it consults the primary —
// the same fallback every URI-less routingProxy method uses. Nil/err-safe:
// returns false whenever the primary is not ready or does not implement pull.
func (r *routingProxy) SupportsPullDiagnostics() bool {
	c, err := r.primaryClient(context.Background())
	if err != nil {
		return false
	}
	pc, ok := c.(pullCapableClient)
	return ok && pc.SupportsPullDiagnostics()
}

// Diagnostic routes the pull request to the adapter owning params' URI and
// delegates. Returns a wrapped error when the routed adapter does not implement
// the pull model, so the diagnostics tool falls back to its push (open-and-wait)
// path rather than surfacing a hard failure.
func (r *routingProxy) Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	c, err := r.route(ctx, params.TextDocument.URI, true)
	if err != nil {
		return nil, err
	}
	pc, ok := c.(pullCapableClient)
	if !ok {
		return nil, fmt.Errorf("pull diagnostics unsupported for %s", params.TextDocument.URI)
	}
	return pc.Diagnostic(ctx, params)
}
