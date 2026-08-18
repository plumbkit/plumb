package cli

// conn_lsp_refresh.go — late primary-language resolution for a live connection.

import (
	"context"

	"github.com/plumbkit/plumb/internal/session"
)

// refreshPrimaryIfStale re-resolves this connection's primary language server
// when the pool's effective language set has widened since the session last
// resolved it — i.e. after a live `plumb enable-lsp <lang>`.
//
// Why it exists: enableLanguage deliberately leaves existing pool entries and
// pinned workspaces untouched, so a session that attached while the language was
// inactive keeps acquiredLanguage == "" for the connection's whole life. Per-file
// routing then lazily attaches the server and answers that session's queries
// correctly, while daemon_info's lsp row and session_start's recommended first
// step keep reporting that no server is attached — steering the agent away from
// the very tools that would work. It self-healed only on a daemon restart, a
// workspace switch, or an explicit session_start({language: …}), none of which an
// agent has reason to try precisely because it was told there is nothing there.
//
// Cost: one atomic load per tool call in the steady state. The generation is
// stored before the work, so a widening costs a connection at most one Detect
// (filesystem-cheap) plus at most one acquireLang, itself bounded by awaitReady's
// startGrace — it never blocks on a full cold start.
//
// Scope: this resolves a MISSING primary only. It never demotes, switches, or
// re-detects an already-attached one, and it deliberately does NOT do what
// attachOrRepinTo does — no read/write/undo tracker resets (that would wipe
// strict-mode state mid-conversation), no quality/topology teardown, and no
// persistPin (the pinned root is unchanged, and re-persisting could demote a
// PinSourceSessionStart origin). Only the language facet moves.
func (s *connSession) refreshPrimaryIfStale(ctx context.Context) {
	if s.pool == nil {
		return
	}
	gen := s.pool.langsGeneration()
	if s.lspGenSeen.Load() == gen {
		return
	}
	// Claim the generation up front: a failed or fruitless refresh must not retry
	// on every subsequent tool call.
	s.lspGenSeen.Store(gen)

	v := s.view()
	if !needsPrimary(v) {
		return
	}
	// Detect must still resolve to the SAME root. A different answer means an
	// ancestor project boundary, not a language that became available — silently
	// moving the pin there is exactly the drift this must never cause.
	root, language, err := s.pool.Detect(v.acquiredRoot)
	if err != nil || root != v.acquiredRoot || language == LanguageNone {
		return
	}

	s.bindRefreshedPrimary(ctx, root, language)
}

// bindRefreshedPrimary binds a primary detected off-lane by
// refreshPrimaryIfStale, provided the session has not moved on since the
// detect. Both re-checks happen inside the mutation lane because each inbound
// MCP message runs in its own goroutine: a concurrent attach may have resolved
// the primary, or a re-pin may have switched the workspace entirely. Binding a
// root the session no longer pins would weld the old project's language onto
// the new workspace — the drift the refresh exists never to cause (the re-pin's
// own attach already resolved the new root against the widened language set).
func (s *connSession) bindRefreshedPrimary(ctx context.Context, root, language string) {
	s.mutate(func(v *sessionView) {
		if !needsPrimary(*v) {
			return // resolved by a concurrent attach while we were detecting
		}
		if v.acquiredRoot != root {
			return // re-pinned to a different workspace while we were detecting
		}
		// repin=false selects the idempotent setPrimary: there is definitively no
		// primary to reset. bindPrimary still records lsRefRoot/lsRefLang, so the
		// pool reference is pinned and released on close exactly as an attach's is.
		lang, adapter, discovered, adapters := s.resolvePrimaryLSP(ctx, v, root, language, false)
		if lang == LanguageNone {
			return // server not installed or failed to start — stay honest
		}
		v.discoveredLangs = distinctLanguages(discovered)
		v.acquiredLanguage = lang
		// The policy is language-scoped (the toolchain dependency roots), so it is
		// rebuilt here and re-folded once warmDepRoots resolves them off-lane —
		// the same order attachWorkspacePin uses.
		v.policy = s.buildPathPolicy(v)
		s.warmDepRoots(lang)
		detectedLanguage := detectedLabel(root, lang, discovered, s.store.Current())
		session.Patch(s.sessionID(), func(info *session.Info) {
			info.Language = lang
			info.DetectedLanguage = detectedLanguage
			info.Adapter = adapter
			info.Adapters = adapters
		})
		s.log().Info("daemon: primary language server resolved after enable-lsp",
			"root", root, "language", lang, "adapter", adapter)
	})
}

// needsPrimary reports whether the view is attached to a workspace but holds no
// primary language server — the only state refreshPrimaryIfStale acts on.
func needsPrimary(v sessionView) bool {
	return v.acquiredRoot != "" && (v.acquiredLanguage == "" || v.acquiredLanguage == LanguageNone)
}
