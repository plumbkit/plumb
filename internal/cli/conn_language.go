package cli

// conn_language.go — what this connection's workspace resolves to in language
// terms: the primary language, the monorepo child languages, the servers that
// have actually served it, and the warm-up/diagnostics state behind them.
//
// Split from conn.go, which holds the session state, the copy-on-write mutation
// lane, and the lifecycle. These are all read-only projections of the acquired
// workspace, and they are what session_start's identity block and the LSP
// notices are rendered from.

import "time"

// acquiredLanguageName returns the LSP language attached to this session, or ""
// when none is (LanguageNone, or not yet attached). session_start uses it to
// distinguish a real "LSP is ready" from a marker-detected project whose
// server is opt-in/off/missing — it must not advertise LSP tools that error.
func (s *connSession) acquiredLanguageName() string {
	lang := s.view().acquiredLanguage
	if lang == "" || lang == LanguageNone {
		return ""
	}
	return lang
}

// acquiredLanguageLabels returns the distinct child languages discovered for a
// monorepo root (the elected primary plus its siblings), as [lsp.<lang>] keys,
// or nil for a single-language root. session_start renders these as the
// "Language: Swift, Zig" identity line; the single primary still drives the
// recommended-step guidance via acquiredLanguageName.
func (s *connSession) acquiredLanguageLabels() []string {
	return s.view().discoveredLangs
}

// lspWarming reports whether this session's primary language server is still
// warming (handshake incomplete) and how long it has been. session_start uses it
// to soften "LSP is ready" into a warming advisory so an agent reaches for
// topology/workspace_symbols meanwhile instead of blocking a semantic tool on a cold
// server. Returns (false, 0) when no language is attached or the server is ready.
func (s *connSession) lspWarming() (bool, time.Duration) {
	if s.acquiredLanguageName() == "" {
		return false, 0
	}
	return s.sessionProxy.WarmupStatus("")
}

// routedLanguageNames returns the non-primary languages whose servers have
// actually served this session (empty when none have). daemon_info and
// session_start pair it with acquiredLanguageName so a connection with no
// primary — a LanguageNone root served purely by per-file routing — stops
// reporting that no language server is attached while one answers its queries.
func (s *connSession) routedLanguageNames() []string {
	if s.sessionProxy == nil {
		return nil
	}
	return s.sessionProxy.routedLanguages()
}

// lspDiagMode reports the resolved diagnostics mode of this session's primary
// language server (push / pull / hybrid / pull-requested-but-unavailable), or ""
// when no server is attached or the mode is not yet resolved. daemon_info and
// session_start surface it — the mode is authoritative negotiation state, never
// inferred from cache contents.
func (s *connSession) lspDiagMode() string {
	if s.acquiredLanguageName() == "" {
		return ""
	}
	return s.sessionProxy.DiagMode("")
}
