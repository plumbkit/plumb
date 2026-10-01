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

// The three accessors below take the workspace session_start resolved for the
// CALLER (the per-agent pin), not the connection's: on a shared connection a
// subagent pinned to a worktree is served by that worktree's language server,
// and describing the connection primary's server instead told it the wrong
// GOWORK, warm-up state and diagnostics mode (issue #546).

// WithLSPWarmup wires an accessor reporting whether the language server serving
// the caller's workspace is still warming (handshake incomplete) and for how
// long. When it reports warming, session_start softens "LSP is ready" into a
// warming advisory that steers the agent to topology/workspace_symbols meanwhile.
// Nil-safe: unset means never warming. Returns the receiver for chaining.
func (t *SessionStart) WithLSPWarmup(fn func(ws string) (bool, time.Duration)) *SessionStart {
	t.lspWarmingFn = fn
	return t
}

// lspWarming reports the warm-up state of the server serving ws, or (false, 0)
// when no accessor is wired.
func (t *SessionStart) lspWarming(ws string) (bool, time.Duration) {
	if t.lspWarmingFn == nil {
		return false, 0
	}
	return t.lspWarmingFn(ws)
}

// WithLSPDiagMode wires an accessor for the resolved diagnostics mode of the
// language server serving the caller's workspace (push / pull / hybrid /
// pull-requested-but-unavailable). session_start surfaces a non-default mode on
// the "LSP is ready" line so an agent knows the server negotiated something
// other than the push default. Nil-safe: unset ⇒ the mode is never shown.
// Returns the receiver for chaining.
func (t *SessionStart) WithLSPDiagMode(fn func(ws string) string) *SessionStart {
	t.lspDiagModeFn = fn
	return t
}

// lspDiagMode returns the diagnostics mode of the server serving ws, or "" when
// no accessor is wired.
func (t *SessionStart) lspDiagMode(ws string) string {
	if t.lspDiagModeFn == nil {
		return ""
	}
	return t.lspDiagModeFn(ws)
}

// WithLSPGoWorkOff wires an accessor for the go.work the language server serving
// the caller's workspace was started with GOWORK=off against ("" when its
// environment was left alone). session_start names it in the identity block: an
// agent in a worktree otherwise has no way to tell why the server answers about
// the worktree while `go` in its own shell, under the same go.work, does not
// (#521). Nil-safe. Returns the receiver for chaining.
func (t *SessionStart) WithLSPGoWorkOff(fn func(ws string) string) *SessionStart {
	t.lspGoWorkFn = fn
	return t
}

// lspGoWorkNote renders the GOWORK=off identity line, ending in a newline, or
// "" when the server serving ws runs with the environment it inherited.
func (t *SessionStart) lspGoWorkNote(ws string) string {
	if t.lspGoWorkFn == nil {
		return ""
	}
	work := t.lspGoWorkFn(ws)
	if work == "" {
		return ""
	}
	return "Go LSP:   runs with GOWORK=off — " + work + " lists another copy of this module " +
		"(set GOWORK in [lsp.go] env to override)\n"
}
