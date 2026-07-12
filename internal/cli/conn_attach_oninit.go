package cli

// conn_attach_oninit.go — the OnInit workspace attach ladder.
//
// Split from conn_attach.go: that file owns the individual attach primitives,
// this one owns the ORDER they are tried in. The order is the security-relevant
// part — getting it wrong sent a relative-path write into the wrong repository —
// so it lives on its own, next to its own tests (conn_attachoninit_test.go).

import (
	"context"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// attachOnInit runs the workspace attach ladder for a newly-initialised
// connection — a fresh client, or the same `plumb serve` proxy reconnecting to a
// restarted daemon. Every rung is first-wins, so the first that resolves a root
// wins and the rest no-op. The order is the contract; see conn_attach_hint.go.
//
// A pin whose origin is session_start deliberately outranks the client's roots.
// Ranking roots first meant a daemon restart silently undid an explicit re-pin —
// and the roots attach then overwrote the stored pin, so a later relative-path
// write resolved against the client's launch directory and landed in the wrong
// repository. Roots is where the client happened to start; a session_start
// workspace is one the caller actually chose.
//
// A roots-origin pin does not outrank roots: there the client is the newer
// authority (it may have switched folders while the daemon was down). A legacy
// row carries no origin and is treated the same way, so upgrading an existing
// database changes nothing until the next deliberate re-pin.
func (s *connSession) attachOnInit(ctx context.Context, request mcp.RequestFn) {
	pinRoot, pinSource, pinOK := s.loadPin()

	// Rung 1: the proxy replayed the caller's session_start workspace. This is the
	// live call's own authority, re-delivered — and unlike the persisted pin it
	// needs no database, so it holds even with [session] persist_state off or the
	// pin row pruned.
	if replayed := s.view().replayedPin; replayed != "" {
		s.attachReplayedPin(ctx, replayed, sessionstate.PinSourceSessionStart)
	}
	// Rung 1b: the same fact from the database, for a proxy that predates the key.
	if s.workspace() == "" && pinOK && pinSource == sessionstate.PinSourceSessionStart {
		s.attachReplayedPin(ctx, pinRoot, pinSource)
	}
	if s.workspace() == "" {
		// Only ask the client for roots when nothing stronger has pinned us —
		// roots/list is a synchronous round-trip that can block.
		s.attachWorkspace(ctx, rootFromRoots(ctx, request))
	}
	if s.workspace() == "" && pinOK {
		// A roots-origin or legacy pin: the rung that keeps a roots-less client
		// (e.g. Claude Desktop) from coming back unpinned after a restart.
		s.attachReplayedPin(ctx, pinRoot, pinSource)
	}
	if s.workspace() == "" {
		// Last resort: the serve proxy's cwd hint (Detect-validated, never
		// persisted as the sticky pin), then first-tool-call path seeding.
		s.attachFromHint(ctx)
	}
}

// attachReplayedPin attaches a workspace restored from a persisted pin, keeping
// the origin the pin was stored with so the attach cannot demote it.
//
// It resolves through repinWorkspaceFrom, not attachWorkspacePin, because that
// path synthesises a root when pool.Detect finds no marker. attachWorkspacePin
// would instead give up and leave the connection unattached, dropping through to
// the roots rung and silently reinstating the very bug this ladder exists to
// prevent — for every project without a .git or .plumb marker.
func (s *connSession) attachReplayedPin(ctx context.Context, root string, origin sessionstate.PinSource) {
	if root == "" {
		return
	}
	if _, err := s.repinWorkspaceFrom(ctx, root, "", origin); err != nil {
		s.log().Warn("daemon: restoring persisted pin failed", "root", root, "err", err)
		return
	}
	s.log().Info("daemon: workspace restored from persisted pin", "root", root, "source", string(origin))
}
