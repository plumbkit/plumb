package cli

// conn_attach_hint.go — the serve-proxy cwd workspace hint.
//
// The daemon deliberately has no os.Getwd() fallback (it is a singleton shared
// across connections, so its working directory is not a per-session signal) —
// but the per-conversation `plumb serve` proxy IS per-session and cwd-aware,
// and transports its working directory in the initialize params' _meta
// (mcp.MetaWorkspaceKey) as an advisory attach hint. The hint enters the attach
// precedence chain (highest wins, every rung first-wins-idempotent):
//
//	explicit session_start workspace arg (re-pin)
//	→ client roots/list
//	→ persisted pin (reconnect replay of this proxy session)
//	→ serve-proxy cwd hint
//	→ first-tool-call path seeding
//
// The persisted pin beats the replayed hint on purpose: the pin records a
// deliberate re-pin away from the proxy's original launch directory. The hint
// is always validated through pool.Detect (marker required, $HOME excluded)
// and never persisted as the sticky pin, so it can inform an attach but never
// overwrite a workspace the caller actually chose.

import "context"

// onWorkspaceHint records the serve proxy's advisory working directory
// transported in the initialize params' _meta. Store-only: it fires
// synchronously during the initialize exchange, before OnInit, and the attach
// itself happens later through attachFromHint so no workspace work ever runs on
// the initialize response path. An empty hint is a no-op.
func (s *connSession) onWorkspaceHint(dir string) {
	if dir == "" {
		return
	}
	s.mutate(func(v *sessionView) { v.workspaceHint = dir })
}

// attachFromHint attaches the workspace from the stored serve-proxy cwd hint —
// the last attach rung before tool-path seeding (see the file header for the
// full precedence chain). A no-op when no hint is stored or a workspace is
// already attached. The hint goes through attachWorkspacePin with
// explicit=false, so it is validated by pool.Detect (marker required, $HOME
// excluded; a Detect failure leaves the connection unattached rather than
// synthesising a root) and is never persisted as the sticky pin — a reconnect
// restores the workspace the caller actually chose.
func (s *connSession) attachFromHint(ctx context.Context) {
	hint := s.view().workspaceHint
	if hint == "" || s.workspace() != "" {
		return
	}
	s.attachWorkspacePin(ctx, "file://"+hint, false)
	if s.workspace() != "" {
		s.log().Info("daemon: workspace attached from serve cwd hint", "cwd", hint, "root", s.workspace())
	}
}

// rootFromHint resolves the stored serve-proxy cwd hint to a detected workspace
// root, or "" when no hint is stored or detection finds no project boundary.
// Read-only — it never attaches; session_start's roots resolver
// (rootFromClient) uses it as the fallback when the client reports no roots.
func (s *connSession) rootFromHint() string {
	hint := s.view().workspaceHint
	if hint == "" {
		return ""
	}
	root, _, err := s.pool.Detect(hint)
	if err != nil {
		return ""
	}
	return root
}
