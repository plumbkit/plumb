package tools

// session_start_workspace.go — how session_start resolves the workspace for a
// call: the attached root is authoritative; an unattached connection attaches
// through the re-pin callback so the pin lands at the layer the caller's
// (possibly declared) identity selects; an already-attached connection re-pins
// only on an explicit argument or language override. Split from
// session_start.go, which holds the tool wiring and the orientation packet.

import (
	"context"
	"encoding/json"
	"fmt"
)

// resolveSessionWorkspace resolves the workspace for this call. repinnedFrom is
// the previous root when an explicit `workspace` argument switched an
// already-pinned connection to a different project; it is empty otherwise.
func (t *SessionStart) resolveSessionWorkspace(ctx context.Context, raw json.RawMessage) (ws string, repinnedFrom string, err error) {
	var a struct {
		Workspace string `json:"workspace"`
		Language  string `json:"language"`
		Force     bool   `json:"force"`
	}
	_ = json.Unmarshal(raw, &a)
	// The daemon's attached root is authoritative. An attached connection is
	// re-pinned or answered from its root below; an UNATTACHED one that declares
	// an identity was deliberately not pre-pinned by onBeforeTool (PLAN-395),
	// so the attach below is the one that routes it to the right layer.
	if t.ws != nil {
		if current := t.ws(ctx); current != "" {
			return t.resolveAttached(ctx, current, a.Workspace, a.Language, a.Force)
		}
	}
	// Not attached yet: honour an explicit arg through the re-pin callback, then
	// ask the client for roots. There is no daemon-cwd fallback — the daemon's
	// working directory is never a reliable per-session signal (it is shared
	// across all connections), and guessing it produced confidently-wrong
	// "workspaces".
	if a.Workspace != "" {
		ws, uerr := t.resolveUnattachedWorkspace(ctx, a.Workspace, a.Language, a.Force)
		return ws, "", uerr
	}
	if t.roots != nil {
		if ws := t.roots(ctx); ws != "" {
			return ws, "", nil
		}
	}
	return "", "", noWorkspaceError()
}

// resolveUnattachedWorkspace attaches a workspace argument on a connection
// that has no attached root yet. It routes through the re-pin callback
// whenever that is wired — PLAN-395: onBeforeTool DEFERS the pre-Execute pin
// for calls that declare an identity, so this is the path that attaches them,
// after withDeclaredAgent has put the declared identity on the ctx and
// repinWorkspaceFrom can route it to the right layer (the agent's shard when
// sharedWith counts this caller as a second agent, the connection itself when
// the caller is the only one known). The old code routed through the callback
// only for a language override and otherwise returned the raw argument,
// relying entirely on the pre-pin — precisely the wrong-layer stamp the
// deferral removes. The resolved root (not the raw argument) coming back keeps
// the displayed workspace consistent with the TUI, memory, and topology, as
// the language branch already did.
func (t *SessionStart) resolveUnattachedWorkspace(ctx context.Context, workspace, language string, force bool) (string, error) {
	if t.repin == nil {
		return workspace, nil
	}
	root, err := t.repin(ctx, workspace, language, force)
	if err != nil {
		if language != "" {
			return "", fmt.Errorf("session_start: pinning %s as %s: %w", workspace, language, err)
		}
		return "", fmt.Errorf("session_start: pinning %s: %w", workspace, err)
	}
	return root, nil
}

// resolveAttached handles session_start on an already-attached connection: an
// explicit workspace arg routes through the re-pin, a language-only arg forces
// the primary on the current root, and a bare call returns the current root.
//
// A same-dir workspace arg still goes through the re-pin: naming the CURRENT
// workspace explicitly is what promotes a roots-held pin to sticky and clears
// a Health mark left by a refused steal attempt (issue #182) —
// short-circuiting on sameDir here made both unreachable for the exact-path
// call. repinExplicit suppresses the "re-pinned" banner when no root actually
// moved. sameDir (os.SameFile) recognises alias spellings of the current root
// — a symlink, a macOS firmlink (/var vs /private/var), a trailing slash —
// while the daemon's sticky guard compares literal resolved roots, so the
// re-pin is handed the pinned spelling, not the alias: the caller is naming
// their OWN workspace, and the alias must not read as a peer steal.
func (t *SessionStart) resolveAttached(ctx context.Context, current, workspace, language string, force bool) (string, string, error) {
	switch {
	case workspace != "":
		requested := workspace
		if sameDir(workspace, current) {
			if t.repin == nil {
				// Legacy wiring without a re-pin callback: naming the current
				// workspace is a no-op, not a conflict.
				return current, "", nil
			}
			requested = current
		}
		return t.repinExplicit(ctx, current, requested, language, force)
	case language != "":
		return t.forceLanguage(ctx, current, language)
	default:
		return current, "", nil
	}
}

// repinExplicit switches an already-pinned connection to a different workspace
// when the caller passes an explicit `workspace` argument. A deliberate
// session_start argument is an unambiguous intent to work elsewhere, so plumb
// honours it (tearing down and re-attaching the new root) instead of refusing —
// otherwise a connection reused across conversations stays welded to the first
// project it touched, with no in-session escape. It also runs when the argument
// names the CURRENT workspace: the daemon-side same-root path is what promotes
// a roots-held pin to sticky and clears a refusal's Health mark (issue #182),
// and the "re-pinned" banner is suppressed below since no root moved. When no
// re-pin callback is wired (older wiring / tests), it falls back to the
// historical refusal.
// force overrides the sticky-pin guard (issue #182): without it the daemon
// refuses the re-pin when the current pin was itself set by an explicit
// session_start, so a peer agent on a multiplexed connection cannot silently
// steal another agent's workspace.
func (t *SessionStart) repinExplicit(ctx context.Context, current, requested, language string, force bool) (string, string, error) {
	if t.repin == nil {
		if t.pinConflict != nil {
			t.pinConflict(requested)
		}
		return "", "", fmt.Errorf(
			"session_start: workspace is already pinned to %s — cannot re-pin to %s in the same connection. To switch projects, start a new MCP connection",
			current, requested,
		)
	}
	newRoot, err := t.repin(ctx, requested, language, force)
	if err != nil {
		return "", "", fmt.Errorf("session_start: re-pinning to %s: %w", requested, err)
	}
	// Suppress the "re-pinned" banner when the requested path resolves to the
	// same root (e.g. a subdir of the current project, or a language-only pin):
	// no project switch actually happened.
	from := current
	if sameDir(newRoot, current) {
		from = ""
	}
	return newRoot, from, nil
}

// forceLanguage re-pins the connection's CURRENT workspace to a forced primary
// language (the session_start `language` arg without a project switch). With no
// re-pin callback wired it ignores the override rather than failing — the
// orientation packet must still return.
func (t *SessionStart) forceLanguage(ctx context.Context, current, language string) (string, string, error) {
	if t.repin == nil {
		return current, "", nil
	}
	if _, err := t.repin(ctx, current, language, false); err != nil {
		return "", "", fmt.Errorf("session_start: pinning language %s: %w", language, err)
	}
	return current, "", nil
}
