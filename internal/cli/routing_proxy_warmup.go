package cli

import (
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/tools"
)

// Warm-up surfacing for the routing proxy: the not-ready error formatting and
// the resolution-only WarmupStatus query a tool or session_start uses to fail
// fast with an elapsed-time advisory instead of blocking on a cold handshake.
// Split from routing_proxy.go to keep that file under the size cap.

// warmingErr formats the error returned when a routed language server's handshake
// has not yet completed. It folds in elapsed warm-up time and points the caller
// at the tools that answer immediately, so an agent retries (or switches to
// topology) rather than reading a bare "not yet ready" as a hard failure. root
// is appended for the per-file routing case; pass "" for the primary.
func warmingErr(elapsed time.Duration, root string) error {
	loc := ""
	if root != "" {
		loc = " for " + root
	}
	if elapsed <= 0 {
		return fmt.Errorf("LSP server not yet ready%s — it is still starting up; retry shortly "+
			"(%s)", loc, tools.ColdLSPToolsHint)
	}
	return fmt.Errorf("LSP server still warming%s (%s elapsed) — retry shortly "+
		"(%s)", loc, roundWarmElapsed(elapsed), tools.ColdLSPToolsHint)
}

// roundWarmElapsed rounds a warm-up duration to a human-friendly precision:
// 100 ms under a second, whole seconds beyond.
func roundWarmElapsed(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(100 * time.Millisecond)
	}
	return d.Round(time.Second)
}

// WarmupStatus reports whether the language server that would serve uri (or the
// connection's primary, when uri is empty) is still warming up, and for how
// long. Resolution-only — it never starts a server — so a tool or session_start
// can fail fast with an elapsed-time advisory rather than block on a cold
// handshake. Returns (false, 0) when the target is ready or cannot be resolved.
func (r *routingProxy) WarmupStatus(uri string) (warming bool, elapsed time.Duration) {
	root, language := r.warmupTarget(uri)
	if root == "" || language == "" || language == LanguageNone {
		return false, 0
	}
	return r.pool.warmupFor(root, language)
}

// DiagMode reports the resolved diagnostics mode of the language server that
// serves uri (or the connection's primary, when uri is empty): one of push /
// pull / hybrid / pull-requested-but-unavailable, or "" when the target is not
// pooled or its mode is not yet resolved. Resolution-only, like WarmupStatus.
func (r *routingProxy) DiagMode(uri string) string {
	root, language := r.warmupTarget(uri)
	if root == "" || language == "" || language == LanguageNone {
		return ""
	}
	return r.pool.diagModeFor(root, language)
}

// GoWorkOff reports the go.work the language server serving the workspace ws
// (the connection's primary, when ws is empty) was started with GOWORK=off
// against (goLSPEnv), or "" when it runs with the environment it inherited.
// For a ws whose server has not started — a subagent's own workspace before its
// first semantic call — it is the decision that server WILL start with, made
// from disk the same way (PR #559 review B2). Resolution-only: it never starts
// a server.
func (r *routingProxy) GoWorkOff(ws string) string {
	root, language := r.workspaceTarget(ws)
	if root == "" || language == "" || language == LanguageNone {
		return ""
	}
	if ws != "" && !r.pool.hasEntry(root, language) {
		return r.pool.plannedGoWorkOff(root, language)
	}
	return r.pool.goWorkOffFor(root, language)
}

// WorkspaceServer reports the language whose server serves the workspace ws
// ("" for none) and whether that server has started. A per-agent re-pin starts
// none; the first call routed to the workspace does. Resolution-only.
func (r *routingProxy) WorkspaceServer(ws string) (language string, started bool) {
	root, language := r.workspaceTarget(ws)
	if root == "" || language == "" || language == LanguageNone {
		return "", false
	}
	return language, r.pool.hasEntry(root, language)
}

// WorkspaceWarmup is WarmupStatus for the server serving the workspace ws
// rather than one file: what session_start reports for an agent pinned away
// from the connection's root (issue #546).
func (r *routingProxy) WorkspaceWarmup(ws string) (warming bool, elapsed time.Duration) {
	root, language := r.workspaceTarget(ws)
	if root == "" || language == "" || language == LanguageNone {
		return false, 0
	}
	return r.pool.warmupFor(root, language)
}

// WorkspaceDiagMode is DiagMode for the server serving the workspace ws, the
// counterpart of WorkspaceWarmup.
func (r *routingProxy) WorkspaceDiagMode(ws string) string {
	root, language := r.workspaceTarget(ws)
	if root == "" || language == "" || language == LanguageNone {
		return ""
	}
	return r.pool.diagModeFor(root, language)
}

// workspaceTarget resolves the (root, language) of the server serving the
// workspace ws: what detection gives ws, which is the key route() gives that
// workspace's own files, or the connection primary when ws is empty. Unlike
// warmupTarget it never falls back to the primary for a ws that does not
// resolve: another workspace's server is the wrong answer about this one.
func (r *routingProxy) workspaceTarget(ws string) (root, language string) {
	if ws == "" {
		return r.warmupTarget("")
	}
	root, language, err := r.pool.Detect(ws)
	if err != nil {
		return "", ""
	}
	return root, language
}

// warmupTarget resolves the (root, language) WarmupStatus inspects for uri: the
// connection primary when uri is empty, else the URI's detected root and
// per-file language (mirroring route()). Falls back to the primary when URI
// resolution fails.
func (r *routingProxy) warmupTarget(uri string) (root, language string) {
	if uri == "" {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.primaryRoot, r.primaryLang
	}
	// One policy resolution for both halves — see resolveFileTarget.
	root, language, err := r.pool.resolveFileTarget(paths.URIToPath(uri))
	if err != nil {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.primaryRoot, r.primaryLang
	}
	return root, language
}
