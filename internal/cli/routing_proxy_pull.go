package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// ─── Pull diagnostics (textDocument/diagnostic, workspace/diagnostic) ────────
//
// routingProxy is the per-connection LSP handle the diagnostics tool is
// constructed with. The tool type-asserts that handle to its pull interfaces
// (DiagnosticsMode / Diagnostic / DiagnosticCapabilities / WorkspaceDiagnostic)
// and consults the per-URI resolved mode to decide between pulling on demand
// and the push open-and-wait path. Pull is a negotiated, config-gated mode
// ([lsp.<lang>] diagnostics): no currently-validated server requires it, and a
// server that advertised it but answers -32601 (typescript-language-server)
// is downgraded to push for the session (see downgradeDiagMode).

// pullCapableClient is the optional document-pull surface an underlying
// adapter may expose. Resolved structurally from the routed lsp.Client.
type pullCapableClient interface {
	SupportsPullDiagnostics() bool
	Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error)
}

// workspacePullCapableClient is the optional workspace-pull surface an adapter
// may expose (only gopls implements it today). Resolved structurally from the
// routed lsp.Client.
type workspacePullCapableClient interface {
	WorkspaceDiagnostic(ctx context.Context, params protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error)
}

// owningKey resolves the (root, language) pool key of the entry that owns uri,
// mirroring route()'s resolution (Detect for the root, file extension for the
// language, primary fallback throughout). An empty uri resolves to the
// connection's primary — the same fallback every URI-less routingProxy method
// uses.
func (r *routingProxy) owningKey(uri string) (root, language string) {
	r.mu.RLock()
	primaryRoot := r.primaryRoot
	primaryLang := r.primaryLang
	r.mu.RUnlock()
	if uri == "" {
		return primaryRoot, primaryLang
	}
	path := paths.URIToPath(uri)
	detRoot, detLang, err := r.pool.Detect(filepath.Dir(path))
	if err != nil {
		return primaryRoot, primaryLang
	}
	targetLang := detLang
	if fl := r.pool.fileLanguage(path); fl != "" {
		targetLang = fl
	}
	if targetLang == "" || targetLang == LanguageNone {
		return primaryRoot, primaryLang
	}
	return detRoot, targetLang
}

// DiagnosticsMode returns the resolved diagnostics mode (push / pull / hybrid /
// pull-requested-but-unavailable) of the pooled entry owning uri, or "" when no
// entry exists or its mode is not yet resolved. An empty uri reports the
// primary's mode. The mode is the negotiation outcome recorded at Initialize —
// never inferred from cache contents.
func (r *routingProxy) DiagnosticsMode(uri string) string {
	root, language := r.owningKey(uri)
	if root == "" || language == "" {
		return ""
	}
	return r.pool.diagModeFor(root, language)
}

// SupportsPullDiagnostics reports whether the connection's primary adapter
// supports the LSP 3.17 pull model. URI-less by nature (kept for back-compat
// with the tool's legacy structural gate), so it consults the primary — the
// same fallback every URI-less routingProxy method uses. Nil/err-safe: returns
// false whenever the primary is not ready or does not implement pull. New
// callers should prefer the per-URI DiagnosticsMode.
func (r *routingProxy) SupportsPullDiagnostics() bool {
	c, err := r.primaryClient(context.Background())
	if err != nil {
		return false
	}
	pc, ok := c.(pullCapableClient)
	return ok && pc.SupportsPullDiagnostics()
}

// DiagnosticCapabilities reports the pull-diagnostics options detail advertised
// by the server owning uri: interFileDependencies (an edit in one file can
// surface diagnostics in others) and workspaceDiagnostics (the server answers
// workspace/diagnostic). Both false when the entry is absent, not ready, or the
// server advertised no options object. An empty uri consults the primary.
func (r *routingProxy) DiagnosticCapabilities(uri string) (interFileDependencies, workspaceDiagnostics bool) {
	root, language := r.owningKey(uri)
	e := r.pool.lookup(root, language)
	if e == nil {
		return false, false
	}
	c := e.proxy.get()
	if c == nil {
		return false, false
	}
	caps := c.Capabilities()
	if caps == nil {
		return false, false
	}
	opts, ok := caps.DiagnosticOptions()
	if !ok || opts == nil {
		return false, false
	}
	return opts.InterFileDependencies, opts.WorkspaceDiagnostics
}

// Diagnostic routes the document pull request to the adapter owning params'
// URI and delegates. Returns a wrapped error when the routed adapter does not
// implement the pull model, so the diagnostics tool falls back to its push
// (open-and-wait) path rather than surfacing a hard failure.
//
// Downgrade: when the server answers method-not-found (-32601) on a connection
// whose negotiated mode is pull or hybrid, the owning entry is flipped to push
// for the session (one log; see downgradeDiagMode) before the error is
// returned — the caller re-reads the mode, sees push, and falls back.
func (r *routingProxy) Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	uri := params.TextDocument.URI
	c, err := r.route(ctx, uri, true)
	if err != nil {
		return nil, err
	}
	pc, ok := c.(pullCapableClient)
	if !ok {
		return nil, fmt.Errorf("pull diagnostics unsupported for %s", uri)
	}
	rep, err := pc.Diagnostic(ctx, params)
	if jsonrpc.IsMethodNotFound(err) {
		root, language := r.owningKey(uri)
		r.pool.downgradeDiagMode(root, language)
	}
	return rep, err
}

// WorkspaceDiagnostic routes a workspace-wide pull to the adapter owning uri
// (empty uri: the primary — the common case, since the request is not
// document-scoped) and delegates. Callers must gate on DiagnosticCapabilities'
// workspaceDiagnostics; an adapter without the surface returns a wrapped
// error. The same -32601 downgrade as Diagnostic applies.
func (r *routingProxy) WorkspaceDiagnostic(ctx context.Context, uri string, params protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
	c, err := r.route(ctx, uri, true)
	if err != nil {
		return nil, err
	}
	wc, ok := c.(workspacePullCapableClient)
	if !ok {
		return nil, fmt.Errorf("workspace pull diagnostics unsupported")
	}
	rep, err := wc.WorkspaceDiagnostic(ctx, params)
	if jsonrpc.IsMethodNotFound(err) {
		root, language := r.owningKey(uri)
		r.pool.downgradeDiagMode(root, language)
	}
	return rep, err
}
