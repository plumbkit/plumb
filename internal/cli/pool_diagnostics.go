package cli

import (
	"encoding/json"
	"log/slog"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Per-connection diagnostics-mode negotiation. plumb decides, per language
// server, whether to consume pushed publishDiagnostics ("push"), pull via
// textDocument/diagnostic ("pull"), or both ("hybrid"), and records
// "pull-requested-but-unavailable" when a configured pull server does not
// advertise the capability. The resolved mode is NEVER inferred from an empty
// cache — it is the outcome of what plumb REQUESTED and what the server
// ADVERTISED, plus an observed push while in pull mode.

// The four resolved-mode vocabulary strings (card product-contract). A pooled
// entry's diagMode holds exactly one of these (or "" before Initialize).
const (
	diagModePush            = "push"
	diagModePull            = "pull"
	diagModeHybrid          = "hybrid"
	diagModePullUnavailable = "pull-requested-but-unavailable"
)

// autoDiagnosticsMode returns the mode plumb negotiates for a language when its
// [lsp.<lang>] diagnostics knob is "auto" (or unset). Every adapter defaults to
// "push" today: push is the safe, universally-supported path, and moving an
// adapter's auto policy to pull requires real-binary evidence recorded on the
// card. This one function is the single, obvious place to change an adapter's
// auto policy.
func autoDiagnosticsMode(_ string) string {
	return diagModePush
}

// resolveRequestedDiagnosticsMode maps the configured [lsp.<lang>] diagnostics
// value to the mode plumb REQUESTS at initialize: "push" or "pull". An empty or
// "auto" value defers to autoDiagnosticsMode; an explicit "push"/"pull" is
// honoured verbatim. Config validation guarantees the input is one of
// "", "auto", "push", "pull", so the default arm only ever handles ""/"auto".
func resolveRequestedDiagnosticsMode(configured, language string) string {
	switch configured {
	case diagModePush, diagModePull:
		return configured
	default: // "" or "auto"
		return autoDiagnosticsMode(language)
	}
}

// resolveDiagMode records e.diagMode after Initialize, from what plumb requested
// and what the server advertised: a push request is always "push"; a pull
// request becomes "pull" when the server advertises diagnosticProvider, else
// "pull-requested-but-unavailable" (with one warning). Guarded by the pool mutex
// like state/startedAt. The "hybrid" transition happens later, in
// diagnosticsHybridFlip, when a push is observed while in pull mode.
func (p *workspacePool) resolveDiagMode(e *poolEntry, ad lsp.Client, requested string) {
	mode := diagModePush
	if requested == diagModePull {
		caps := ad.Capabilities()
		if caps != nil && caps.PullDiagnosticsEnabled() {
			mode = diagModePull
		} else {
			mode = diagModePullUnavailable
			slog.Warn("pool: pull diagnostics requested but server does not advertise diagnosticProvider — falling back",
				"root", e.root, "language", e.language)
		}
	}
	p.mu.Lock()
	e.diagMode = mode
	p.mu.Unlock()
}

// diagnosticsHybridFlip returns a notification handler that flips a "pull"
// connection to "hybrid" the first time a pushed publishDiagnostics is observed
// — evidence the server is dual-mode. Every other mode is left untouched. It is
// subscribed alongside the invalidator so the flip is driven by the same push
// stream. Guarded by the pool mutex like state/startedAt; the log fires after
// the lock is released, and only on the (single) transition.
func (p *workspacePool) diagnosticsHybridFlip(e *poolEntry) func(string, json.RawMessage) {
	return func(method string, _ json.RawMessage) {
		if method != protocol.MethodPublishDiagnostics {
			return
		}
		p.mu.Lock()
		flipped := e.diagMode == diagModePull
		if flipped {
			e.diagMode = diagModeHybrid
		}
		p.mu.Unlock()
		if flipped {
			slog.Info("pool: pull server also pushed diagnostics — recording hybrid mode",
				"root", e.root, "language", e.language)
		}
	}
}

// diagModeFor returns the resolved diagnostics mode of the pooled (root,
// language) entry, or "" when no such entry exists (or its mode is not yet
// resolved). Read under the pool mutex. Surfacing calls this; it never infers a
// mode from cache contents.
func (p *workspacePool) diagModeFor(root, language string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[poolKey{root, language}]
	if !ok {
		return ""
	}
	return e.diagMode
}
