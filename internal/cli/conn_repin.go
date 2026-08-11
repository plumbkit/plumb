package cli

// conn_repin.go — the deliberate workspace re-pin machinery and the sticky-pin
// guard (issue #182). Split from conn_attach.go by responsibility: that file
// owns first attach and language detection; this one owns how an already-
// attached connection switches (or refuses to switch) workspaces.

import (
	"context"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools/txlog"
)

// repinWorkspace deliberately switches the connection to a different workspace.
// Unlike attachWorkspace (idempotent, first-wins — the safe default for
// auto-resolution), this is driven only by an explicit session_start workspace
// argument: an unambiguous declaration of intent. It tears down the previous
// workspace's per-session subsystems (quality runner, topology store, LSP
// routing) and re-attaches the new root, so a connection reused across
// conversations (e.g. Claude Desktop) is no longer permanently welded to the
// first project it touched. The ad-hoc boundary guard on other path tools is
// unaffected — only this deliberate bootstrap call re-pins.
//
// folder may be any absolute path inside the target project. It is resolved to
// a workspace root via pool.Detect; when no marker is found the folder itself
// becomes the workspace (SynthesiseRoot), so an explicit pin always succeeds.
// Returns the resolved root.
//
// langOverride, when a non-empty active language, forces the primary language
// instead of the detected one — for an ambiguous project (e.g. an Xcode app with
// no SwiftPM Package.swift) where the agent knows the language detection cannot
// infer. An unknown or inactive override is ignored (detection wins), so a typo
// or an uninstalled server never breaks the pin.
//
// force overrides the sticky-pin guard (issue #182): once this connection's pin
// was set by an explicit session_start, a conflicting re-pin to a DIFFERENT root
// is refused unless force is set — a multiplexed peer agent sharing this
// connection must not silently steal the pin the caller deliberately chose.
// The refusal error names the remediation (retry with force: true), so a new
// conversation deliberately switching projects still can, one round-trip later.
func (s *connSession) repinWorkspace(ctx context.Context, folder, langOverride string, force bool) (string, error) {
	return s.repinWorkspaceFrom(ctx, folder, langOverride, sessionstate.PinSourceSessionStart, pinTriggerLive, force)
}

// repinWorkspaceFrom is repinWorkspace with the pin origin made explicit.
// onRootsChanged shares this machinery but is NOT a session_start re-pin: the
// client moved its folder, which is a weaker signal than a workspace the caller
// named. Recording both as session_start would let a stale roots answer outrank
// a deliberate pin on the next reconnect — the bug this distinction exists to
// prevent.
//
// The returned root is the REQUESTED folder's resolved root. On the one path
// where the request is silently not honoured — a live roots-driven re-pin kept
// off an explicit pin — it therefore differs from the pin the connection still
// holds; the only such caller (onRootsChanged) discards the value.
func (s *connSession) repinWorkspaceFrom(ctx context.Context, folder, langOverride string, origin sessionstate.PinSource, trigger pinTrigger, force bool) (string, error) {
	folder = paths.URIToPath(folder)
	if folder == "" || folder == "/" {
		return "", fmt.Errorf("repin: empty workspace path %q", folder)
	}
	root, language, err := s.pool.Detect(folder)
	synthetic := err != nil
	if synthetic {
		// No .plumb/marker/.git found — the folder itself becomes the workspace.
		root = s.pool.SynthesiseRoot(folder)
		language = LanguageNone
	}
	langForced := false
	if langOverride != "" && s.pool.hasActiveLanguage(langOverride) {
		language = langOverride
		langForced = true
	}
	// The sticky-pin guard (issue #182) lives inside attachOrRepinTo's mutation
	// lane: after the root resolution above, so a requested path that resolves
	// to the current root is never falsely refused, and on the view under
	// mutation, so a concurrent re-pin can never land between the refusal
	// decision and the pin move.
	changed, err := s.attachOrRepinTo(ctx, root, language, origin, trigger, force, synthetic, langForced)
	if err != nil {
		return "", err
	}
	if changed {
		s.applyProjectConfig(root)
	}
	return root, nil
}

// attachOrRepinTo points the connection at root, tearing down any previous
// workspace's per-session subsystems first so the start* helpers (which no-op
// when already started) re-create them for the new root. Returns changed=true
// when the root actually changed (false on a no-op re-pin to the same root).
// language is the LSP language for root, or LanguageNone. The whole
// teardown-and-reattach runs under the one mutation lane so readers never see
// a half-switched view.
//
// The sticky-pin guard (issue #182) is enforced here, inside the mutation
// lane: a non-nil error means the re-pin was refused and the pin is untouched.
// A live roots-driven re-pin against an explicit pin is instead a silent keep
// (changed=false, refused=nil, pin untouched). Checking on the view under
// mutation (rather than a snapshot taken before the lane) closes the
// check-then-act window — two racing explicit re-pins serialise, the first
// lands and makes the pin sticky, and the second is refused rather than
// silently replacing it.
//
// synthetic records whether root was synthesised (no marker found), so the
// session record keeps its Synthetic flag truthful across re-pins and restore
// replays instead of hardcoding false. langForced marks language as an
// explicit, active session_start override — the only signal allowed to
// re-acquire on a same-root call (see the no-op branch).
func (s *connSession) attachOrRepinTo(ctx context.Context, root, language string, origin sessionstate.PinSource, trigger pinTrigger, force, synthetic, langForced bool) (changed bool, refused error) {
	s.mutate(func(v *sessionView) {
		prev := v.acquiredRoot
		// Sticky-pin guard (issue #182). Only a LIVE re-pin away from a pin held
		// by an explicit session_start is gated: a same-root request falls
		// through to the promotion branch below, a restore replay is never
		// blocked (the pin's owner re-attaching is not a peer stealing it), and
		// a roots/auto-attach pin is not sticky — the first explicit pin must
		// always land.
		if !force && trigger == pinTriggerLive && prev != "" && root != prev &&
			v.pinOrigin == sessionstate.PinSourceSessionStart {
			if origin == sessionstate.PinSourceSessionStart {
				s.log().Warn("daemon: session_start re-pin refused — explicit pin held (sticky, issue #182)", "pinned", prev, "requested", root)
				// Surface the refused steal attempt to the operator (TUI /
				// dashboard); a later successful re-pin clears Health below.
				s.markBoundaryViolation(fmt.Sprintf("session_start re-pin refused: explicit pin %s is sticky; requested %s (issue #182)", prev, root))
				refused = fmt.Errorf("refusing to re-pin this connection from %s to %s: the current pin was set by an explicit session_start (%s), and silently moving it would retarget every relative-path call made over this shared connection — issue #182: a multiplexing client can run several agent sessions over one plumb serve process. If you are a new conversation deliberately switching this connection to a different project, call session_start again with force: true; if several agents share this connection, run a dedicated plumb serve process per agent instead", prev, root, pinProvenanceOf(v))
				return
			}
			// A roots-driven re-pin (the client dropped our root from its
			// reported set) is a weaker signal than the deliberate pin: keep the
			// pin, no error — the live counterpart of the persisted-pin
			// promotion rule. onRootsChanged short-circuits this case up front;
			// this in-lane check is the authoritative one.
			s.log().Info("daemon: roots re-pin skipped — explicit session_start pin held (issue #182)", "pinned", prev, "requested", root)
			return
		}
		// No-op when the root does not change, UNLESS the caller explicitly
		// forced a different primary language (an active session_start
		// `language` arg) — only that declared intent re-acquires on the same
		// root. Detect-vs-acquired drift — a failed LSP acquire (acquired
		// none), a monorepo root electing a child primary, a content-sniffed
		// root — must never let a REDUNDANT same-root session_start take the
		// teardown path and reset the read/write/undo trackers.
		if root == prev && (!langForced || language == v.acquiredLanguage) {
			// Nothing to re-acquire — but an explicit session_start still UPGRADES
			// the pin's recorded origin. Without this, a session_start(workspace=B)
			// for a B already attached from client roots would leave the stored
			// origin as roots, and a later restart whose client roots point
			// elsewhere would beat the deliberate pin — so the persisted-pin channel
			// would not be correct on its own (independent of the proxy replay).
			// Promotion is one-way: a same-root roots notification (origin !=
			// session_start) is skipped here, so it can never demote a session_start
			// pin.
			if origin == sessionstate.PinSourceSessionStart {
				// Persist the language actually acquired, not Detect's raw value
				// — on this branch they can differ (drift cases above).
				s.persistPin(v.proxySessionID, root, v.acquiredLanguage, v.session.PersistState, origin)
				// The root did not move, so pinAt/pinPrev stand; only the provenance
				// origin/label is upgraded — making the pin sticky from here on.
				// Rebuild the policy so boundary errors quote the new label.
				v.pinVia = pinViaLabel(origin, trigger)
				v.pinOrigin = origin
				v.policy = s.buildPathPolicy(v)
				// A successful explicit session_start naming the CURRENT root is
				// the natural health probe: clear a "blocked" mark left by a
				// refused steal attempt (or a fumbled path), just as the
				// root-changed branch below does — the victim's own re-pin must
				// heal the session, not only a forced switch.
				session.Patch(s.sessID, func(info *session.Info) {
					info.Health = ""
					info.HealthMessage = ""
				})
			}
			return
		}
		changed = true
		// The pinned LS reference (if any) for the workspace we are leaving;
		// released at the end once the new root is acquired, so the pool can reclaim
		// the old server after its idle grace if no other session holds it.
		prevRef := v.lsRefRoot
		prevRefLang := v.lsRefLang
		v.lsRefRoot = ""
		v.lsRefLang = ""
		if v.qualityRunner != nil {
			v.qualityRunner.Stop()
			v.qualityRunner = nil
		}
		v.topologyStore = nil // pool stores are daemon-lifetime and shared; just re-Acquire
		// Per-session read/write tracking is workspace-relative: plumb has read and
		// written nothing in the new project yet, so the dirty-guard and strict-mode
		// read check must start clean rather than inherit the old root's paths.
		s.readTracker.Reset()
		s.writeTracker.Reset()
		s.undoStore.Reset()
		s.clearHintSeen()

		lang, adapter, discovered, adapters := s.resolvePrimaryLSP(ctx, v, root, language, true)
		language = lang
		// Acquire-before-release: the new root is pinned above before we drop the
		// old one, so even a re-pin back to a recently-left root never races teardown.
		if prevRef != "" {
			s.pool.release(prevRef, prevRefLang)
		}
		detectedLanguage := detectedLabel(root, language, discovered, s.store.Current())
		v.discoveredLangs = distinctLanguages(discovered)
		v.acquiredRoot = root
		v.acquiredLanguage = language
		recordPinProvenance(v, origin, trigger, prev)
		v.lastCfgMtime = time.Time{}
		// Rehydrate AFTER the Reset() above, keyed by the NEW root, so a re-pin to a
		// different workspace can never restore the old project's reads. Re-persist
		// the pin for the switched-to root.
		s.rehydrateReads(v.proxySessionID, root, v.session.PersistState)
		s.persistPin(v.proxySessionID, root, language, v.session.PersistState, origin)
		s.startQualityRunner(v, root)
		s.startTopologyIndexer(v, root)
		v.policy = s.buildPathPolicy(v)
		s.warmDepRoots(language)
		recoverWorkspaceTxlog(root, func(ws string) { txlog.Scan(ws, s.daemonStartedAt, txlogReplayGuard(v.policy)) })
		cn, cv := v.clientName, v.clientVersion
		session.Patch(s.sessID, func(info *session.Info) {
			info.Folder = root
			info.Language = language
			info.DetectedLanguage = detectedLanguage
			info.Adapter = adapter
			info.Adapters = adapters
			info.Synthetic = synthetic
			info.Health = ""
			info.HealthMessage = ""
			if cn != "" {
				info.ClientName = cn
				info.ClientVersion = cv
			}
		})
		s.log().Info("daemon: session re-pinned", "from", prev, "to", root, "language", language, "adapter", adapter,
			"source", pinSourceLabel(origin), "trigger", string(trigger))
	})
	return changed, refused
}
