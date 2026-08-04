package gopls

import (
	"context"
	"maps"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// goplsOptions holds gopls-specific initialization options.
// See https://github.com/golang/tools/blob/master/gopls/doc/settings.md
type goplsOptions struct {
	Analyses      map[string]bool `json:"analyses,omitempty"`
	StaticCheck   bool            `json:"staticcheck,omitempty"`
	Hints         map[string]bool `json:"hints,omitempty"`
	VerboseOutput bool            `json:"verboseOutput,omitempty"`
	// PullDiagnostics enables gopls's experimental LSP 3.17 pull model
	// (textDocument/diagnostic). Off by default (gopls is push-first); set only
	// when the resolved [lsp.go] diagnostics mode is "pull" via
	// EnablePullDiagnostics.
	PullDiagnostics bool `json:"pullDiagnostics,omitempty"`
}

// Adapter implements lsp.Client for gopls.
//
// The 23 lsp.Client methods come from base.Adapter, which labels each error
// "gopls <label>: <cause>". On top of those this package declares gopls's own
// pull-diagnostics surface: EnablePullDiagnostics (the only lsp.PullInitializer
// in the tree), SupportsPullDiagnostics, Diagnostic and WorkspaceDiagnostic.
// They are declared HERE, not on the base, because an exported base method is
// promoted into all nine adapters at once and plumb resolves these capabilities
// structurally (see the base package doc).
//
// Concurrency: all exported methods are safe for concurrent use.
// Capabilities() is safe to call concurrently with any other method.
type Adapter struct{ *base.Adapter }

// Compile-time contract check: a mis-signed method fails here, in this package,
// rather than as a confusing error wherever the adapter is used as an lsp.Client.
// gopls is the only adapter that also customises its own pull negotiation, and
// internal/cli resolves that structurally, so pin it at build time too.
var (
	_ lsp.Client          = (*Adapter)(nil)
	_ lsp.PullInitializer = (*Adapter)(nil)
)

// New creates an Adapter wired to conn. The caller must call Initialize before
// any query method.
func New(conn jsonrpc.Caller) *Adapter {
	return &Adapter{Adapter: base.New(conn, "gopls")}
}

// EnablePullDiagnostics reconfigures params for the LSP 3.17 pull-diagnostics
// model: it advertises the pull client capability (via
// protocol.ClientCapabilitiesFor) and sets gopls's experimental
// "pullDiagnostics" initialization option so gopls answers
// textDocument/diagnostic. It implements lsp.PullInitializer; the pool calls it
// only when the resolved diagnostics mode for Go is "pull". Pull is additive:
// the push capability is preserved, so gopls keeps pushing publishDiagnostics
// too (the pool records "hybrid" if it does). Safe on any params shape: the
// typed goplsOptions default gets the flag set directly; a user-configured
// [lsp.go] initialization_options map gets the flag injected into a clone
// (unless the user set the key themselves); anything else only swaps the
// client capability.
func (a *Adapter) EnablePullDiagnostics(params *protocol.InitializeParams) {
	params.Capabilities = protocol.ClientCapabilitiesFor(true)
	switch opts := params.InitializationOptions.(type) {
	case goplsOptions:
		opts.PullDiagnostics = true
		params.InitializationOptions = opts
	case map[string]any:
		// A user-configured [lsp.go] initialization_options table replaces the
		// typed defaults, but pull mode still needs gopls's experimental flag —
		// inject it into a clone (never mutating the user's config map) unless
		// the user set the key themselves, either way.
		if _, set := opts["pullDiagnostics"]; !set {
			merged := maps.Clone(opts)
			merged["pullDiagnostics"] = true
			params.InitializationOptions = merged
		}
	}
}

// DefaultInitParams returns InitializeParams suitable for gopls.
func DefaultInitParams(rootURI string) protocol.InitializeParams {
	return protocol.InitializeParams{
		ProcessID:    protocol.ProcessID(),
		ClientInfo:   &protocol.ClientInfo{Name: "plumb", Version: "dev"},
		RootURI:      rootURI,
		Capabilities: protocol.DefaultClientCapabilities(),
		InitializationOptions: goplsOptions{
			Analyses: map[string]bool{
				"unusedresult": true,
				"unusedparams": true,
			},
			StaticCheck: false,
		},
	}
}

// ── Diagnostics (pull) ───────────────────────────────────────────────────────

// SupportsPullDiagnostics reports whether gopls advertised the
// textDocument/diagnostic pull model at initialize. gopls is push-first — it
// reports diagnostics through textDocument/publishDiagnostics and does not
// advertise diagnosticProvider under plumb's negotiated capabilities — so this
// returns false in practice and the diagnostics tool stays on the push path for
// Go. The method exists so the session proxy can route pull uniformly across
// adapters; it is gated on the advertised capability, never on a guess. Returns
// false before Initialize. Mirrors the zls adapter.
func (a *Adapter) SupportsPullDiagnostics() bool {
	caps := a.Capabilities()
	return caps != nil && caps.PullDiagnosticsEnabled()
}

// Diagnostic requests diagnostics for a single document via the LSP 3.17 pull
// model (textDocument/diagnostic). Callers should gate this on
// SupportsPullDiagnostics: gopls only advertises (and answers) it when the
// resolved [lsp.go] diagnostics mode is "pull" (EnablePullDiagnostics); under
// the default push negotiation this path stays dormant. Mirrors the zls
// adapter.
func (a *Adapter) Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	result, err := base.CallPtr[protocol.DocumentDiagnosticReport](ctx, a.Adapter, "diagnostic", protocol.MethodDiagnostic, params)
	if err != nil {
		return nil, err
	}
	normaliseDiagnosticReport(result)
	return result, nil
}

// normaliseDiagnosticReport contains a verified gopls wire quirk at the adapter
// boundary. gopls v0.23 omits kind on an otherwise-valid clean full report; the
// generic pull cache intentionally rejects unknown kinds because it cannot
// safely infer them for arbitrary servers.
func normaliseDiagnosticReport(result *protocol.DocumentDiagnosticReport) {
	if result != nil && result.Kind == "" {
		result.Kind = protocol.DiagnosticReportFull
	}
}

// WorkspaceDiagnostic requests diagnostics for the whole workspace via
// workspace/diagnostic (LSP 3.17). Callers must gate on the server advertising
// DiagnosticOptions.WorkspaceDiagnostics (see ServerCapabilities); a server
// without it answers -32601, which the pool treats as a downgrade signal. Only
// reachable when pull diagnostics were negotiated.
func (a *Adapter) WorkspaceDiagnostic(ctx context.Context, params protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
	return base.CallPtr[protocol.WorkspaceDiagnosticReport](ctx, a.Adapter, "workspace diagnostic", protocol.MethodWorkspaceDiagnostic, params)
}
