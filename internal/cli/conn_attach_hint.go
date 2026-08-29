package cli

// conn_attach_hint.go — the serve workspace hint.
//
// The daemon deliberately has no os.Getwd() fallback (it is a singleton shared
// across connections, so its working directory is not a per-session signal).
// The per-conversation `plumb serve` proxy is per-session, but its working
// directory is equally not a declaration of intent — an MCP client spawns serve
// from its own launcher's directory — so serve transports only an EXPLICIT
// workspace pre-pin, the --workspace flag or PLUMB_WORKSPACE env var resolved
// by resolveWorkspaceHint, in the initialize params' _meta
// (mcp.MetaWorkspaceKey). A serve started with neither sends no key at all and
// starts UNATTACHED — no workspace, no pin — leaving session_start({workspace})
// as the sole pin authority for such serves. The hint enters the attach
// precedence chain (highest wins, every rung first-wins-idempotent):
//
//	explicit session_start workspace arg — live, or replayed by the proxy in
//	  _meta[mcp.MetaPinnedWorkspaceKey] after a daemon restart. The replayed
//	  form keeps this RANK but not the full authority: the daemon cannot
//	  authenticate the _meta channel, so it does not carry #306's home
//	  containment exemption at attach or on a policy rebuild (issue #318).
//	→ persisted pin whose source is session_start (this proxy session)
//	→ client roots/list
//	→ persisted pin whose source is roots, or a legacy row with no source
//	→ serve workspace hint (--workspace/PLUMB_WORKSPACE)
//	→ first-tool-call path seeding
//
// A session_start-origin pin outranks client roots because it is the workspace
// the caller actually chose, whereas roots is only where the client happened to
// launch. Ranking roots first meant a deliberate re-pin was silently undone by
// every daemon restart — and worse, the roots attach then overwrote the stored
// pin, so a relative-path write landed in the wrong repository. A roots-origin
// pin does NOT outrank a fresh roots answer: there the client is the newer
// authority. A legacy row predates the discriminator and is treated as roots, so
// upgrading changes no behaviour until the next deliberate re-pin.
//
// The persisted pin beats the replayed hint on purpose: the pin records a
// deliberate re-pin away from the serve's pre-pin. The hint is always validated
// through pool.Detect (marker required, $HOME excluded) and never persisted as
// the sticky pin, so it can inform an attach but never overwrite a workspace
// the caller actually chose.

import (
	"context"

	"github.com/plumbkit/plumb/internal/sessionstate"
)

// onWorkspaceHint records the workspace pre-pin the serve proxy transported in
// the initialize params' _meta — the explicit --workspace/PLUMB_WORKSPACE value
// (resolveWorkspaceHint); the serve's working directory is never transported.
// Store-only: it fires synchronously during the initialize exchange, before
// OnInit, and the attach itself happens later through attachFromHint so no
// workspace work ever runs on the initialize response path. An empty hint is a
// no-op.
func (s *connSession) onWorkspaceHint(dir string) {
	if dir == "" {
		return
	}
	s.mutate(func(v *sessionView) { v.workspaceHint = dir })
}

// onPinnedWorkspace records the workspace the caller chose with an explicit
// session_start, replayed by the serve proxy after a daemon restart. Store-only
// and fires before OnInit, exactly like onWorkspaceHint — but this one is
// authoritative rather than advisory, and the ladder consults it first. An empty
// value (a first connect, or a proxy that predates the key) is a no-op.
func (s *connSession) onPinnedWorkspace(dir string) {
	if dir == "" {
		return
	}
	s.mutate(func(v *sessionView) { v.replayedPin = dir })
}

// attachFromHint attaches the workspace from the stored serve workspace hint —
// the explicit --workspace/PLUMB_WORKSPACE pre-pin, the last attach rung before
// tool-path seeding (see the file header for the full precedence chain). A
// no-op when no hint is stored (an unattached serve: session_start pins
// instead) or a workspace is already attached. The hint goes through
// attachWorkspacePin with explicit=false, so it is validated by pool.Detect
// (marker required, $HOME excluded; a Detect failure leaves the connection
// unattached rather than synthesising a root) and is never persisted as the
// sticky pin — a reconnect restores the workspace the caller actually chose.
func (s *connSession) attachFromHint(ctx context.Context) {
	hint := s.view().workspaceHint
	if hint == "" || s.workspace() != "" {
		return
	}
	s.attachWorkspacePin(ctx, "file://"+hint, sessionstate.PinSourceUnknown)
	if s.workspace() != "" {
		s.log().Info("daemon: workspace pre-pinned from the serve workspace hint (--workspace/PLUMB_WORKSPACE)", "hint", hint, "root", s.workspace())
	}
}

// rootFromHint resolves the stored serve workspace hint (the explicit
// --workspace/PLUMB_WORKSPACE pre-pin) to a detected workspace root, or "" when
// no hint is stored (an unattached serve) or detection finds no project
// boundary. Read-only — it never attaches; session_start's roots resolver
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
