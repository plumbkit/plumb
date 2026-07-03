package cli

// conn_persist.go — persistence of per-connection state across daemon restarts.
//
// When `plumb daemon` restarts, the resilient `plumb serve` proxy stays
// connected and replays the captured initialize handshake, which carries the
// proxy's stable session ID (onProxySession). The fresh daemon uses that ID to
// recognise the reconnected connection as a continuation of the previous one and
// rehydrate the state that would otherwise be lost: strict-mode read tracking
// and (for clients that do not report roots) the pinned workspace.
//
// Everything here is gated on [session].persist_state, a non-nil store, a
// non-empty proxy session ID, and a pinned workspace; any of those missing makes
// the call a no-op, so a non-serve client or a disabled feature behaves exactly
// as before.

import (
	"context"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
)

// onProxySession records the stable proxy session ID transported in the
// initialize params' _meta. It fires synchronously during the initialize
// exchange, before OnInit attaches the workspace, so the ID is present when
// attachWorkspace rehydrates.
func (s *connSession) onProxySession(id string) {
	if id == "" {
		return
	}
	s.mutate(func(v *sessionView) { v.proxySessionID = id })
}

// persistRead is the ReadTracker sink: it mirrors a recorded read to the durable
// store, scoped by (proxy session ID, workspace), so strict mode survives a
// daemon restart. Reads the live view per call (cheap atomic load) so it always
// uses the current workspace and never resurrects reads for a different project.
func (s *connSession) persistRead(path string, mtime time.Time, sha string) {
	v := s.view()
	if !s.persistEnabled(v) {
		return
	}
	if err := s.sessionState.UpsertRead(v.proxySessionID, v.acquiredRoot, path, mtime, sha); err != nil {
		s.log().Debug("daemon: persist read failed", "err", err)
	}
}

// persistEnabled reports whether per-connection state should be persisted for
// the given view: the feature is on, the store opened, and both the proxy
// session ID and a workspace are known.
func (s *connSession) persistEnabled(v sessionView) bool {
	return s.sessionState != nil && v.session.PersistState && v.proxySessionID != "" && v.acquiredRoot != ""
}

// rehydrateReads loads the persisted reads for (proxyID, root) into the live
// read tracker, so a strict-mode edit of a file read before a daemon restart is
// not refused for "not read this session". Keyed by the freshly-pinned root, so
// it can never restore reads from a different workspace. Called from inside the
// attach mutation lane with the working view's fields, hence the explicit args
// (s.view() inside mutate would see the pre-swap snapshot).
func (s *connSession) rehydrateReads(proxyID, root string, persistState bool) {
	if s.sessionState == nil || !persistState || proxyID == "" || root == "" {
		return
	}
	recs, err := s.sessionState.LoadReads(proxyID, root)
	if err != nil {
		s.log().Debug("daemon: rehydrate reads failed", "err", err)
		return
	}
	if len(recs) == 0 {
		return
	}
	out := make([]tools.ReadRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, tools.ReadRecord{Path: r.Path, Mtime: r.Mtime, SHA: r.SHA})
	}
	s.readTracker.Hydrate(out)
	s.log().Info("daemon: rehydrated read-tracking", "root", root, "count", len(out))
}

// persistPin records the pinned workspace (and language) for the proxy session,
// so a client that does not report roots (e.g. Claude Desktop) comes back pinned
// after a restart. Called from inside the attach mutation lane with explicit
// args, for the same reason as rehydrateReads.
//
// Only an EXPLICIT pin is persisted: a deliberate session_start workspace arg or
// a client-reported root. An auto-attach seeded from an incidental tool path
// (onBeforeTool) passes explicit=false and writes nothing, so it can never
// overwrite the sticky target — a reconnect then lands back on the last
// workspace the caller actually chose rather than on whatever file it read
// first. This closes the silent pin-drift where reading a file in project B by
// absolute path re-pinned a connection away from the explicitly-chosen A.
func (s *connSession) persistPin(proxyID, root, language string, persistState, explicit bool) {
	if !explicit {
		return
	}
	if s.sessionState == nil || !persistState || proxyID == "" || root == "" {
		return
	}
	if err := s.sessionState.UpsertPin(proxyID, root, language); err != nil {
		s.log().Debug("daemon: persist pin failed", "err", err)
	}
}

// rehydratePin re-pins the workspace from a persisted pin when the connection
// came back unpinned (the client reported no roots). Idempotent: attachWorkspace
// is first-wins, so it no-ops if a root was already pinned. Called from OnInit
// after the normal attach attempt.
func (s *connSession) rehydratePin(ctx context.Context) {
	v := s.view()
	if s.sessionState == nil || !v.session.PersistState || v.proxySessionID == "" {
		return
	}
	root, _, ok, err := s.sessionState.LoadPin(v.proxySessionID)
	if err != nil {
		s.log().Debug("daemon: load pin failed", "err", err)
		return
	}
	if !ok || root == "" {
		return
	}
	s.attachWorkspace(ctx, "file://"+root)
	if s.workspace() != "" {
		s.log().Info("daemon: workspace rehydrated from persisted pin", "root", root)
	}
}
