package cli

// conn_roots.go — the client roots/list_changed handling.
//
// Split from conn_attach.go by responsibility: this owns how a client's reported
// workspace roots map to (and, on change, re-pin) the connection's workspace.
// The load-bearing rule lives here — a mere reorder of a multi-root list must not
// drift the pin (issue #182) — so it sits with its own tests (conn_rootsrotation_test.go).

import (
	"context"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// onRootsChanged applies a client's updated workspace roots (the
// notifications/roots/list_changed path). On the first attach it pins the root,
// like OnInit. When the connection is already pinned and the client reports a
// different root — an editor that genuinely switched folders — it re-pins to
// follow the switch, closing the same "welded connection" gap that the
// session_start re-pin fixed for clients that never report roots (Claude
// Desktop). An empty or unchanged root is left alone: repinWorkspace no-ops when
// the resolved root matches the current pin, so a spurious notification (or a
// roots/list the client cannot satisfy) never tears the workspace down.
func (s *connSession) onRootsChanged(ctx context.Context, roots []string) {
	if len(roots) == 0 {
		return // client reported no usable roots — keep the current pin
	}
	if s.view().acquiredRoot == "" {
		s.attachWorkspace(ctx, roots[0])
		s.applyProjectConfig(s.workspace())
		return
	}
	// A multi-root client that merely REORDERS its roots (or adds/removes OTHER
	// folders) must not drag the pin between projects. Taking Roots[0] on every
	// notification did exactly that — issue #182's roots-rotation drift, with no
	// session_start in between. Keep the pin while its root is still reported; only
	// a genuine removal of our workspace re-pins.
	if s.pinnedRootStillReported(roots) {
		return
	}
	folder := paths.URIToPath(roots[0])
	if folder == "" || folder == "/" {
		return
	}
	if _, err := s.repinWorkspaceFrom(ctx, folder, "", sessionstate.PinSourceRoots); err != nil {
		s.log().Warn("daemon: roots-changed re-pin failed", "to", folder, "err", err)
	}
}

// pinnedRootStillReported reports whether the currently-pinned workspace root is
// still derivable from any of the client's reported roots, resolved the same way
// a re-pin would — so a reported subfolder that Detects up to the pinned root
// counts as "still reported".
func (s *connSession) pinnedRootStillReported(roots []string) bool {
	cur := s.workspace()
	if cur == "" {
		return false
	}
	for _, r := range roots {
		folder := paths.URIToPath(r)
		if folder == "" || folder == "/" {
			continue
		}
		if s.resolveRootFolder(folder) == cur {
			return true
		}
	}
	return false
}

// resolveRootFolder resolves an absolute folder to its workspace root the same
// way repinWorkspaceFrom does: the detected project root, or the folder itself
// synthesised as a root when no marker is found. Kept in step with that resolution.
func (s *connSession) resolveRootFolder(folder string) string {
	root, _, err := s.pool.Detect(folder)
	if err != nil {
		return s.pool.SynthesiseRoot(folder)
	}
	return root
}
