package typescript

import (
	"context"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Adapter implements lsp.Client for typescript-language-server.
//
// typescript-language-server expects a rootUri pointing at the project root
// (the directory containing tsconfig.json / jsconfig.json / package.json) and
// drives tsserver underneath. It registers file watchers dynamically via
// client/registerCapability, which the embedded base answers so
// DidChangeWatchedFiles events are filtered to the registered globs.
//
// The 23 lsp.Client methods are inherited from base.Adapter, which labels each
// error "typescript-language-server <label>: <cause>". On top of those this
// package declares the optional document-pull surface
// (SupportsPullDiagnostics + Diagnostic) — declared HERE, not on the base,
// because an exported base method is promoted into all nine adapters at once
// and would opt servers that answer -32601 to textDocument/diagnostic into the
// pull model (see the base package doc).
//
// Concurrency: all exported methods are safe for concurrent use.
type Adapter struct{ *base.Adapter }

// Compile-time contract check: a mis-signed method fails here, in this package,
// rather than as a confusing error wherever the adapter is used as an lsp.Client.
var _ lsp.Client = (*Adapter)(nil)

// New creates an Adapter wired to conn. The caller must call Initialize before
// any query method.
func New(conn jsonrpc.Caller) *Adapter {
	return &Adapter{Adapter: base.New(conn, "typescript-language-server")}
}

// DefaultInitParams returns InitializeParams suitable for
// typescript-language-server. rootURI must be a file:// URI pointing to the
// project root. No initialization options are required for plumb's use.
func DefaultInitParams(rootURI string) protocol.InitializeParams {
	return protocol.InitializeParams{
		ProcessID:    protocol.ProcessID(),
		ClientInfo:   &protocol.ClientInfo{Name: "plumb", Version: "dev"},
		RootURI:      rootURI,
		Capabilities: protocol.DefaultClientCapabilities(),
	}
}

// ── Diagnostics (pull) ─────────────────────────────────────────────────────────

// SupportsPullDiagnostics reports whether the server advertised the
// textDocument/diagnostic pull model during initialize. In practice
// typescript-language-server (≥ 5.3) advertises no diagnosticProvider and returns
// -32601 for textDocument/diagnostic, so this is false for it: the server publishes
// diagnostics over the push stream instead, once the client declares the
// textDocument.publishDiagnostics capability (see DefaultClientCapabilities).
// Returns false before Initialize.
func (a *Adapter) SupportsPullDiagnostics() bool {
	caps := a.Capabilities()
	return caps != nil && caps.PullDiagnosticsEnabled()
}

// Diagnostic requests diagnostics for a single document via the LSP 3.17 pull
// model (textDocument/diagnostic). Callers should gate this on
// SupportsPullDiagnostics; a server that only pushes will return an error here.
func (a *Adapter) Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	return base.CallPtr[protocol.DocumentDiagnosticReport](ctx, a.Adapter, "diagnostic", protocol.MethodDiagnostic, params)
}
