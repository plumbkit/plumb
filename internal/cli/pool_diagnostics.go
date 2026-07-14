package cli

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
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

// clearEntryPullState drops any pull-diagnostics result IDs and pull snapshots
// held by an entry's Invalidator, so a freshly started — or just-woken — server
// process never matches a previousResultId it did not issue (a stale ID could
// elicit a false "unchanged", and with it a false clean, violating the safety
// invariant). Push diagnostics are left intact: a fresh server re-publishes
// them.
//
// This is the single (re)start seam. It is invoked from poolOnStart, which the
// supervisor re-runs on BOTH a fresh start and every wake from hibernation
// (wakeLocked → sup.StartAsync re-runs the captured OnStart), so both cases are
// covered without taking the Invalidator lock under the pool mutex — keeping the
// pool→invalidator direction lock-free and preserving the existing lock
// discipline. A config reload does not restart the server (the [lsp.<lang>]
// diagnostics knob is ReloadNextSession, so a running server's negotiated mode
// and its result IDs stay valid), and a re-pin either reuses a live server
// (whose IDs are valid) or wakes one (covered here); neither needs its own
// clear. State is per poolEntry, so unrelated roots and languages are untouched.
func clearEntryPullState(e *poolEntry) {
	if e.inv != nil {
		e.inv.ClearPullState()
	}
}

// downgradeDiagMode flips an entry to "push" for the rest of the session after a
// negotiated pull returned method-not-found (-32601): the server advertised the
// capability at initialize but does not actually answer textDocument/diagnostic,
// so plumb stops pulling and falls back to the push open-and-wait path. It logs
// exactly once (only on the transition off a non-push mode) and is a no-op when
// the entry is already push or absent. Guarded by the pool mutex like
// resolveDiagMode. The tool re-reads the mode after a failed pull: seeing "push"
// tells it a downgrade occurred so it falls back rather than surfacing an error.
func (p *workspacePool) downgradeDiagMode(root, language string) {
	p.mu.Lock()
	e, ok := p.entries[poolKey{root, language}]
	if !ok || (e.diagMode != diagModePull && e.diagMode != diagModeHybrid) {
		// Only a negotiated pull connection downgrades: push stays push, and
		// "" / pull-requested-but-unavailable keep their (surfaced) states.
		p.mu.Unlock()
		return
	}
	prev := e.diagMode
	e.diagMode = diagModePush
	p.mu.Unlock()
	slog.Warn("pool: pull diagnostics returned method-not-found — downgrading to push for this session",
		"root", root, "language", language, "from", prev)
}

// wrapServerRequest layers workspace/diagnostic/refresh handling in FRONT of an
// adapter's own server-request handler (inner). On a refresh request it drops
// the entry's pull result IDs and snapshots (clearEntryPullState) so the NEXT
// diagnostics query re-pulls without a stale previousResultId, then answers the
// request PROMPTLY with a null result. It never performs a blocking workspace
// pull inside the JSON-RPC read loop — the server would deadlock waiting on a
// response it is itself blocking. Every other method delegates to inner (the
// shared register/unregister-capability handling), so per-adapter behaviour is
// preserved exactly. This is the single wiring point for refresh across ALL
// adapters (poolOnStart calls it), so no adapter threads an extension itself.
func (p *workspacePool) wrapServerRequest(e *poolEntry, inner jsonrpc.RequestHandler) jsonrpc.RequestHandler {
	return func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if method == protocol.MethodWorkspaceDiagnosticRefresh {
			clearEntryPullState(e) // next query re-pulls; do NOT block the read loop
			return nil, nil
		}
		if inner != nil {
			return inner(ctx, method, params)
		}
		return nil, &jsonrpc.MethodNotFoundError{Method: method}
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
