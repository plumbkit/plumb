package cli

// conn_persist.go — persistence of per-connection state across daemon restarts.
//
// When `plumb daemon` restarts, the resilient `plumb serve` proxy stays
// connected and replays the captured initialize handshake, which carries the
// proxy's stable session ID (onProxySession). The fresh daemon uses that ID to
// recognise the reconnected connection as a continuation of the previous one and
// rehydrate the state that would otherwise be lost: strict-mode read tracking,
// and (for clients that do not report roots) the pinned workspace.
//
// The connection's IDENTITY — its internal session ID, its name, and its
// authorised external linkage — is recovered by conn_restore.go, which owns that
// whole lifecycle. This file keeps the expendable state: the caches whose loss
// costs a re-read or a re-pin rather than a forked identity. The two are
// deliberately separate, because they have opposite failure rules. Losing a
// cache is fine and expiring one is correct; doing either to the identity record
// forks the session.
//
// Everything here is gated on [session].persist_state, a non-nil store, and a
// non-empty proxy session ID (reads and pins additionally need a pinned
// workspace); any of those missing makes the call a no-op, so a non-serve
// client or a disabled feature behaves exactly as before.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// onProxySession records the stable proxy session ID transported in the
// initialize params' _meta. It fires synchronously during the initialize
// exchange, before OnInit attaches the workspace, so the ID is present when
// attachWorkspace rehydrates. It then restores the session's identity from the
// record that ID selects — identity is workspace-independent, so it needs none
// of the attach state, which means the initialize RESULT can already state who
// this session is and the first session_start already answers under the
// restored name.
func (s *connSession) onProxySession(id string) {
	if id == "" {
		return
	}
	s.mutate(func(v *sessionView) { v.proxySessionID = id })
	s.restoreIdentity(id)
}

// onSessionID records the plumb session ID the serve proxy replayed in the
// initialize _meta (mcp.MetaSessionIDKey) — the identity a reconnecting session
// believed it held before the daemon restarted.
//
// It is now purely a CLAIM to be reconciled, not an input to recovery. Recovery
// resolves the identity from the durable record the proxy secret selects
// (restoreIdentity, which OnProxySession fires first), and that record is
// authoritative. This matters beyond tidiness: a serve process built before the
// ID was ever echoed replays nothing at all, and gating recovery on the replay
// left exactly those long-lived proxies — the ones a restart is most likely to
// catch — unable to come back as themselves.
//
// A mismatch is reported rather than converged. The proven record stands; the
// stale claim is logged so a genuine inconsistency is visible instead of being
// silently resolved in favour of whichever value was written last.
func (s *connSession) onSessionID(id string) {
	if id == "" {
		return
	}
	s.mutate(func(v *sessionView) { v.replayedSessionID = id })
	proven := s.view().persistedIdentity.SessionID
	if proven == "" || proven == id {
		return
	}
	s.log().Warn("daemon: the replayed session ID disagrees with the proven durable record; keeping the proven identity",
		"replayed", id, "proven", proven, "using", s.sessionID())
}

// inheritSessionID accepts a predecessor's plumb session ID as a second mailbox
// identity for this session, so messages BOUND to the session a daemon restart
// ended still reach the agent they were written for. Without it, binding a
// message to a session — which is what stops a name-reuser reading it — would
// also strand every unread message across a restart, since the reconnected
// connection registers under a fresh session ID.
//
// It is called from exactly one place, and that is the whole security argument.
// The grant is authorised by the PROXY session ID: a 122-bit random value the
// serve process generates for itself, replays only inside its own initialize
// handshake, and which plumb never writes to a session file, a log line, or any
// tool result. Presenting it is evidence of being the same serve process; being
// called "alice" is not. Inheriting on the strength of a name would hand any
// session its predecessor's mailbox for the cost of one rename_session, which is
// precisely the hole the binding closed.
//
// Inheritance is the DEGRADED path: adoption (same record, same authorisation)
// is primary, and on a successful restore the session reads its own mail under
// its own ID, so no inherit grant is made at all. What remains here is the case
// adoption was DECLINED — a live overlap, or an Adopt error — where the session
// runs under a temporary ID and still needs its predecessor's mail.
//
// It is also gated on the rename having SUCCEEDED, so a session only inherits an
// identity while actually holding the name that identity answered to.
//
// The chain is bounded at ONE predecessor, deliberately. A later legitimate
// rename re-records this session's OWN ID under the proxy key, so the chain
// never grows into an ever-widening set of identities one session may read.
func (s *connSession) inheritSessionID(prevID string) {
	if prevID == "" || prevID == s.sessionID() {
		return
	}
	s.mutate(func(v *sessionView) { v.inheritedSessionIDs = []string{prevID} })
	s.log().Debug("daemon: inherited predecessor mailbox identity", "predecessor", prevID)
}

// inheritedSessionIDs returns the predecessor identities this session may also
// read mail for. Nil for every session that did not come back through the
// authenticated persisted-state path, which is the overwhelming majority.
func (s *connSession) inheritedSessionIDs() []string {
	return s.view().inheritedSessionIDs
}

// persistIdentity commits this session's CURRENT identity — its name, its own
// session ID, and its external linkage — to the durable record under the proxy
// session ID, so the next reconnect can recover it.
//
// Recording this session's own ID, never an inherited one, is what bounds the
// inheritance chain at a single predecessor: after a second restart the ID
// stored here is this session's, and the one it inherited is forgotten.
//
// The callers are deliberately few, and every one of them is a case where the
// stored record is genuinely out of date: first contact, a legitimate rename, an
// external-ID link, and the one repair case where the stored name is invalid by
// construction. Recovery FAILURES do not call it — see conn_restore.go, where
// that used to be the identity-fork bug.
// It reports whether the identity is now durably recorded. Callers that are
// about to CLAIM durable continuity must consult it: advertising a recoverable
// identity that was never committed is the one thing worse than not having one,
// because the agent then relies on a name and ID that will not come back.
func (s *connSession) persistIdentity() bool {
	v := s.view()
	name := v.sessName
	if !s.namePersistEnabled(v) || name == "" {
		return false
	}
	if v.recovery == recoveryDegraded {
		// This connection is running under a TEMPORARY identity: recovery found
		// the record and could not apply it. Writing now records the stand-in
		// over the proven identity — the fork the refusal paths exist to avoid,
		// arriving through whichever caller happens to persist next. session_start's
		// external-ID linker is that caller in practice, and it is the first call
		// most agents make.
		//
		// The guard lives HERE rather than at each call site precisely because the
		// last two rounds of this fix each closed one path and left another open.
		s.log().Debug("daemon: not recording a temporary identity over the proven durable record",
			"temporary", s.sessionID(), "proven", v.persistedIdentity.SessionID)
		return false
	}
	rec := sessionstate.Identity{
		Name:       name,
		SessionID:  s.sessionID(),
		ExternalID: s.externalID(),
	}
	if err := s.sessionState.SaveIdentity(v.proxySessionID, rec); err != nil {
		s.log().Debug("daemon: persist session identity failed", "err", err)
		return false
	}
	return true
}

// externalID returns the external-conversation ID linked to this session, or ""
// when it has none. It is read from the session file rather than cached on the
// view because session_start writes it there directly (session.SetExternalID),
// and a second copy is a second thing that can go stale.
func (s *connSession) externalID() string {
	id := s.sessionID()
	if id == "" {
		return ""
	}
	return session.ExternalIDOf(id)
}

// namePersistEnabled is persistEnabled without the workspace requirement: a
// session's identity is workspace-independent, so it can be loaded and saved
// during the initialize exchange, before a workspace is known.
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
// Only a pin with a known origin is persisted: a deliberate session_start call
// (a workspace arg, or a language override applied to the current root — both
// record session_start origin), or a client-reported root. An auto-attach
// seeded from an
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
//
// The pin is verified before it is restored (restoreRootIntact): a persisted
// root whose directory was deleted — a removed git worktree is the standing
// case — otherwise rehydrates to the nearest ANCESTOR that looks like a
// project, silently widening the session's write surface past anything the
// caller chose (the #181 fail-open class through the restore path).
func (s *connSession) rehydratePin(ctx context.Context) {
	root, source, ok := s.loadPin()
	if !ok {
		return
	}
	resolved, synthetic, intact := s.restoreRootIntact(root)
	if !intact {
		s.dropPin(root, source)
		return
	}
	if synthetic {
		// A persisted markerless (synthetic-root) pin: Detect finds no marker
		// now either, so attachWorkspacePinFrom would defer to the first tool
		// call and the deliberate pin would fall through to the weaker
		// cwd-hint/seed rungs. Re-synthesise under the loaded origin instead —
		// attachSynthetic is first-wins, preserving this function's idempotence.
		//
		// explicit only when the STORED origin is session_start: replaying a pin
		// does not upgrade its provenance, so a $HOME row an earlier build
		// persisted from a weaker source cannot ride the replay back into being
		// the workspace. A row the caller genuinely created with
		// session_start({workspace: "~"}) still restores — issue #182.
		//
		// The re-synthesis itself is the load-bearing step, not its argument:
		// restoreRootIntact asked the home-directory question with explicit=true
		// (it has no origin), which cannot refuse a $HOME row persisted by an
		// earlier build from a weaker source — only THIS call, with the row's
		// stored origin, can. Seeding it from `resolved` rather than the raw
		// stored root is behaviourally neutral: restoreRootIntact keeps a
		// markerless root only when it synthesises to ITSELF, so the two are
		// equal by construction here — it just keeps the call honest about which
		// spelling was verified.
		synth := s.pool.SynthesiseRoot(resolved, source == sessionstate.PinSourceSessionStart)
		if synth == "" {
			s.log().Warn("daemon: not restoring persisted pin — it names the home directory and was not set by an explicit session_start", "root", resolved, "source", string(source))
			return
		}
		// issue #347: SynthesiseRoot walks up to the nearest .git same as Detect
		// does, so a marker appearing above `resolved` between restoreRootIntact's
		// call and this one climbs synth to that ancestor. resolved == root here
		// (restoreRootIntact only keeps a markerless root when it synthesises to
		// itself), so synth != resolved IS the drift — refuse rather than attach
		// an ancestor nothing verified.
		if err := restoreDriftErr(resolved, synth, pinTriggerRestore); err != nil {
			s.log().Warn("daemon: not restoring persisted pin — re-synthesising it drifted to a different root than the one verified", "verified", resolved, "resolved", synth, "source", string(source))
			return
		}
		s.attachSynthetic(ctx, synth, source, pinTriggerRestore)
	} else {
		s.attachWorkspacePinFrom(ctx, "file://"+resolved, source, pinTriggerRestore)
	}
	if s.workspace() != "" {
		s.log().Info("daemon: workspace rehydrated from persisted pin", "root", resolved, "source", string(source))
	}
}

// restoreRootIntact verifies a persisted or replayed pin root before it is
// restored: the directory must still exist, and resolving it must land back on
// EXACTLY the stored root — never on an ancestor the detect walk would climb
// to, and never on a different canonical spelling. Both mismatches are dropped
// rather than attached: the first widens the write surface (a deleted worktree
// rehydrating to the enclosing repo), the second would pin the project under a
// spelling the caller never held, shadowing the canonical pin.
//
// synthetic reports whether the root resolves markerless (no .plumb/marker/.git
// anywhere up the chain), so the caller attaches it via attachSynthetic exactly
// as the original pin did. The comparison is against the stored string, not its
// canonicalisation: every pin plumb persists is already the canonical resolved
// root, so an alias-spelled row is by definition one plumb did not write — drop
// it and let the normal attach ladder re-pin canonically.
func (s *connSession) restoreRootIntact(root string) (resolved string, synthetic, intact bool) {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", false, false
	}
	if detected, _, derr := s.pool.Detect(root); derr == nil {
		if detected != root {
			return "", false, false
		}
		return detected, false, true
	}
	// Markerless pin: SynthesiseRoot walks up to the nearest .git, so it has the
	// same widening shape as Detect's climb — only a root that synthesises to
	// itself is restored.
	//
	// explicit=true here, and it changes nothing about what is restored: the
	// result is kept only when it equals the stored root, so the home-directory
	// refusal (which returns "") could only turn an intact pin into a dropped
	// one. Whether a $HOME row may be restored at all is rehydratePin's decision,
	// made from the row's STORED origin; this function answers the narrower
	// question of whether the directory still resolves to itself, and asking the
	// origin question twice — in a helper that does not have the origin — is how
	// the last six review rounds each found the next gap.
	if synth := s.pool.SynthesiseRoot(root, true); synth == root {
		return synth, true, true
	}
	return "", false, false
}

// restoreDriftErr reports a restore whose re-resolution landed somewhere other
// than the root restoreRootIntact already verified. restoreRootIntact answers
// "does this root still resolve to itself?" and then THROWS THE ANSWER AWAY —
// every restore-path caller re-runs pool.Detect or pool.SynthesiseRoot on the
// verified root to recover the language or the synthetic/non-synthetic split,
// a second uncached filesystem walk that can disagree with the first if a
// marker is added or removed above the root in the interval between the two
// (issue #347). undeclaredWideRootErr exempts PinSourceSessionStart entirely,
// so on the restore path nothing else stands between that second answer and
// attach — the premise "the string checked is the string attached" holds only
// because restoreRootIntact refuses drift; this closes the gap where a
// caller re-derives instead of reusing its answer. A live trigger has no such
// premise to protect and is never gated: rung 1b's whole purpose is a
// caller-declared WIDE root, so this refuses DRIFT, not width.
func restoreDriftErr(verified, resolved string, trigger pinTrigger) error {
	if trigger != pinTriggerRestore || resolved == verified {
		return nil
	}
	return fmt.Errorf("restore: re-resolving %s landed on %s instead of the root that was verified — refusing rather than attaching a root nothing checked (issue #347)", verified, resolved)
}

// dropPin refuses to restore a pin whose root failed verification, deletes the
// persisted row so the same drop is not re-attempted on every reconnect and
// every unpinned tool call, and leaves the connection unattached — the normal
// attach ladder (roots, cwd hint, tool-path seeding) still runs, and when
// nothing lower resolves the session stays honestly unattached
// (UnattachedWorkspaceError) instead of silently pinned to a wider root. The
// drop is logged at Warn: a healthy rehydrate logs at Info, and this is not one.
func (s *connSession) dropPin(root string, source sessionstate.PinSource) {
	s.log().Warn("daemon: persisted pin dropped — root no longer resolves to itself; refusing to widen to an ancestor",
		"root", root, "source", string(source))
	v := s.view()
	if s.sessionState == nil || !v.session.PersistState || v.proxySessionID == "" {
		return
	}
	// Delete only the row that names THIS root: a replayed proxy pin (rung 1)
	// and the persisted row (rung 1b) record the same fact, but a stale replay
	// must not delete a newer pin the daemon stored after the proxy learned its.
	stored, _, _, ok, err := s.sessionState.LoadPin(v.proxySessionID)
	if err != nil || !ok || stored != root {
		return
	}
	if err := s.sessionState.DeletePin(v.proxySessionID); err != nil {
		s.log().Debug("daemon: delete persisted pin failed", "err", err)
	}
}

// toolResultMeta contributes `_meta` to successful tool results. It carries two
// facts, and they are gated differently on purpose.
//
// The session ID (mcp.MetaSessionIDKey) rides EVERY successful session_start.
// It used to ride only a call that carried a workspace argument, which coupled
// identity to workspace selection: `session_start({session_id})` — the exact
// call an agent makes to link a conversation without re-pinning — told the
// proxy nothing, so the proxy held no identity and the connection could not
// prove itself on the next reconnect. Nothing about identity depends on a
// workspace, and the two must not share a gate.
//
// The resolved workspace (mcp.MetaResolvedWorkspaceKey) still rides only a call
// that carried a workspace argument, because it answers "what did that argument
// resolve to?" and there is no argument otherwise. It is the CANONICAL root the
// daemon actually pinned, so the proxy commits the resolved spelling instead of
// the caller's raw one: an alias-spelled pin could shadow a project under two
// roots, and a replayed subdirectory would re-resolve against a fresh daemon
// whose state the proxy knows nothing about. The workspace is read after the
// call, when onBeforeTool/repin has already attached the resolved root.
//
// Emitting the ID for a no-workspace call is only half the repair; the proxy
// must also record it without disturbing a pin the call never mentioned. See
// commitSessionStartPin.
func (s *connSession) toolResultMeta(_ context.Context, name string, args json.RawMessage) map[string]any {
	if name != sessionStartTool {
		return nil
	}
	meta := map[string]any{}
	if id := s.sessionID(); id != "" {
		meta[mcp.MetaSessionIDKey] = id
	}
	if ws := s.workspace(); ws != "" && workspaceArgPresent(args) {
		meta[mcp.MetaResolvedWorkspaceKey] = ws
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}
