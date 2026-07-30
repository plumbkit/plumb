package cli

// conn_persist.go — persistence of per-connection state across daemon restarts.
//
// When `plumb daemon` restarts, the resilient `plumb serve` proxy stays
// connected and replays the captured initialize handshake, which carries the
// proxy's stable session ID (onProxySession). The fresh daemon uses that ID to
// recognise the reconnected connection as a continuation of the previous one and
// rehydrate the state that would otherwise be lost: strict-mode read tracking,
// (for clients that do not report roots) the pinned workspace, and the session
// name (mailbox notes are addressed by name, so a rename on every reconnect
// would orphan them).
//
// Everything here is gated on [session].persist_state, a non-nil store, and a
// non-empty proxy session ID (reads and pins additionally need a pinned
// workspace); any of those missing makes the call a no-op, so a non-serve
// client or a disabled feature behaves exactly as before.

import (
	"context"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// onProxySession records the stable proxy session ID transported in the
// initialize params' _meta. It fires synchronously during the initialize
// exchange, before OnInit attaches the workspace, so the ID is present when
// attachWorkspace rehydrates. It also restores the persisted session name here
// — the name is workspace-independent, so it needs none of the attach state —
// which means the first session_start already answers under the restored name.
func (s *connSession) onProxySession(id string) {
	if id == "" {
		return
	}
	s.mutate(func(v *sessionView) { v.proxySessionID = id })
	s.restoreName(id)
}

// restoreName applies the name persisted under this proxy session ID, so a
// reconnect after a daemon restart keeps the same session name. The gate is
// namePersistEnabled — deliberately persistEnabled minus the workspace
// requirement, since restoreName runs during initialize, before any workspace
// is known. A first-seen proxy ID records the freshly generated name so the
// NEXT reconnect can restore it. Applying the name goes through renameSession,
// keeping the session file, view, and stats store consistent (and re-saving
// the name, which refreshes its TTL).
func (s *connSession) restoreName(id string) {
	v := s.view()
	if !s.namePersistEnabled(v) {
		return
	}
	name, ok, err := s.sessionState.LoadName(id)
	if err != nil {
		s.log().Debug("daemon: load session name failed", "err", err)
		return
	}
	if !ok {
		s.persistName(v.sessName)
		return
	}
	// A proxy reconnect can overlap its predecessor (the proxy reconnected but the
	// previous connSession is still registered), and session.Rename enforces no
	// uniqueness. Two live sessions under one name would make leave_note delivery
	// — which matches on the name string — ambiguous, so keep the generated name
	// this time and leave the stored one for the next reconnect.
	if nameHeldByOtherLiveSession(name, s.sessID) {
		s.log().Debug("daemon: persisted session name still held by a live session; keeping the generated name",
			"persisted", name, "using", v.sessName)
		return
	}
	if _, err := s.renameSession(name); err != nil {
		s.log().Debug("daemon: restore session name failed", "name", name, "err", err)
	}
}

// nameHeldByOtherLiveSession reports whether a live session other than selfID
// already answers to name. Best-effort: an unreadable session directory reports
// false, so a restore is never blocked by a transient listing failure.
func nameHeldByOtherLiveSession(name, selfID string) bool {
	infos, err := session.List()
	if err != nil {
		return false
	}
	for _, info := range infos {
		if info.Name == name && info.ID != selfID {
			return true
		}
	}
	return false
}

// persistName records the session's current name under its proxy session ID.
func (s *connSession) persistName(name string) {
	v := s.view()
	if !s.namePersistEnabled(v) || name == "" {
		return
	}
	if err := s.sessionState.SaveName(v.proxySessionID, name); err != nil {
		s.log().Debug("daemon: persist session name failed", "err", err)
	}
}

// namePersistEnabled is persistEnabled without the workspace requirement: the
// session name is workspace-independent, so it can be loaded and saved during
// the initialize exchange, before a workspace is known.
func (s *connSession) namePersistEnabled(v sessionView) bool {
	return s.sessionState != nil && v.session.PersistState && v.proxySessionID != ""
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
// Only a pin with a known origin is persisted: a deliberate session_start
// workspace arg, or a client-reported root. An auto-attach seeded from an
// incidental tool path (onBeforeTool) passes PinSourceUnknown and writes
// nothing, so it can never overwrite the sticky target — a reconnect then lands
// back on the last workspace the caller actually chose rather than on whatever
// file it read first. This closes the silent pin-drift where reading a file in
// project B by absolute path re-pinned a connection away from the
// explicitly-chosen A.
//
// The origin is stored, not just used as a gate: on reconnect only a
// session_start-origin pin outranks the client's roots. See conn_attach_hint.go.
func (s *connSession) persistPin(proxyID, root, language string, persistState bool, origin sessionstate.PinSource) {
	if origin == sessionstate.PinSourceUnknown {
		return
	}
	if s.sessionState == nil || !persistState || proxyID == "" || root == "" {
		return
	}
	if err := s.sessionState.UpsertPin(proxyID, root, language, origin); err != nil {
		s.log().Debug("daemon: persist pin failed", "err", err)
	}
}

// loadPin returns this proxy session's persisted pin, if any. Gated exactly like
// persistPin, so a disabled store or a store-less session simply reports "none".
func (s *connSession) loadPin() (root string, source sessionstate.PinSource, ok bool) {
	v := s.view()
	if s.sessionState == nil || !v.session.PersistState || v.proxySessionID == "" {
		return "", sessionstate.PinSourceUnknown, false
	}
	root, _, source, ok, err := s.sessionState.LoadPin(v.proxySessionID)
	if err != nil {
		s.log().Debug("daemon: load pin failed", "err", err)
		return "", sessionstate.PinSourceUnknown, false
	}
	if !ok || root == "" {
		return "", sessionstate.PinSourceUnknown, false
	}
	return root, source, true
}

// rehydratePin re-pins the workspace from a persisted pin when the connection
// came back unpinned. Idempotent: attachWorkspacePin is first-wins, so it no-ops
// if a root was already pinned.
//
// It re-persists the pin under the origin it was LOADED with. Re-attaching via
// attachWorkspace would stamp PinSourceRoots over a session_start row, quietly
// demoting a deliberate pin to one that no longer outranks the client's roots —
// the pin would then lose the very next reconnect.
func (s *connSession) rehydratePin(ctx context.Context) {
	root, source, ok := s.loadPin()
	if !ok {
		return
	}
	s.attachWorkspacePin(ctx, "file://"+root, source)
	if s.workspace() != "" {
		s.log().Info("daemon: workspace rehydrated from persisted pin", "root", root, "source", string(source))
	}
}
