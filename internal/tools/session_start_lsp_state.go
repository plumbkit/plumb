package tools

import "time"

// LSP-state accessors for the orientation packet, split out of session_start.go
// to keep it under the file-size cap. These report what the session's language
// servers are actually doing — attached, routed, still warming, and which
// diagnostics mode was negotiated — so orientation describes the live state
// rather than an assumed one.

// lspAttached reports whether a language server is attached for this session.
func (t *SessionStart) lspAttached() bool {
	return t.lspLangFn != nil && t.lspLangFn() != ""
}

// WithLSPRouted wires an accessor for the non-primary languages whose servers
// have actually served this session through per-file routing. A workspace with
// no detectable primary language (a bare .plumb/ root) never attaches one, so
// without this the recommended first step told the agent no language server was
// attached while routing was answering its queries — and the agent, believing
// it, stopped asking. Nil-safe. Returns the receiver for chaining.
func (t *SessionStart) WithLSPRouted(fn func() []string) *SessionStart {
	t.lspRoutedFn = fn
	return t
}

// lspRouted returns the routed (non-primary) languages serving this session, or
// nil when none are or no accessor is wired.
func (t *SessionStart) lspRouted() []string {
	if t.lspRoutedFn == nil {
		return nil
	}
	return t.lspRoutedFn()
}

// WithLSPWarmup wires an accessor reporting whether the session's primary
// language server is still warming (handshake incomplete) and for how long. When
// it reports warming, session_start softens "LSP is ready" into a warming
// advisory that steers the agent to topology/workspace_symbols meanwhile. Nil-safe:
// unset means never warming. Returns the receiver for chaining.
func (t *SessionStart) WithLSPWarmup(fn func() (bool, time.Duration)) *SessionStart {
	t.lspWarmingFn = fn
	return t
}

// lspWarming reports the primary LSP warm-up state, or (false, 0) when no
// accessor is wired.
func (t *SessionStart) lspWarming() (bool, time.Duration) {
	if t.lspWarmingFn == nil {
		return false, 0
	}
	return t.lspWarmingFn()
}

// WithLSPDiagMode wires an accessor for the resolved diagnostics mode of this
// session's primary language server (push / pull / hybrid /
// pull-requested-but-unavailable). session_start surfaces a non-default mode on
// the "LSP is ready" line so an agent knows the connection negotiated something
// other than the push default. Nil-safe: unset ⇒ the mode is never shown.
// Returns the receiver for chaining.
func (t *SessionStart) WithLSPDiagMode(fn func() string) *SessionStart {
	t.lspDiagModeFn = fn
	return t
}

// lspDiagMode returns the primary LSP's resolved diagnostics mode, or "" when no
// accessor is wired.
func (t *SessionStart) lspDiagMode() string {
	if t.lspDiagModeFn == nil {
		return ""
	}
	return t.lspDiagModeFn()
}
