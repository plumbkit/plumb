package cli

// conn_resume_accept.go — presenting a resume credential (design §3 "Acceptance" and
// "Grant equivalence", §5 "Rotation", "Revocation", "Exclusive-owner fencing" and
// "Replay after rotation is a theft signal").
//
// A replacement `plumb serve` holds no proxy credential the daemon recognises, so its
// connection is a first contact (established) and the external-conversation id it then
// names recovers only the NAME: that id is client-supplied, a claim, and decision D1
// forbids letting a claim authorise an internal session ID. A presented resume
// credential is the one new authority, and it is equivalent to the proxy credential:
// when its hash matches the CURRENT generation of an identity linked to the named
// conversation, the connection escalates into a full restore of the internal session
// ID, the name, the mail bound to the ID and the thread seats (the conn_restore.go
// machinery, run from a second trigger), the identity is re-recorded under THIS
// serve's proxy credential, and the credential rotates in the same response.
//
// Everything is gated so that a presentation proves nothing unless it should:
//
//   - Only the OWNER of the connection's conversation, on a connection that is a FRESH
//     proven identity (recovery established, a proxy credential, persistence on) and
//     not linked to some other conversation. A subagent, another conversation, an
//     ordinary MCP client, a connection that is degraded or already restored: ignored,
//     and the call behaves exactly as it does today.
//   - Only a CURRENT generation restores. A superseded one is a replay (refused, and
//     logged loudly: a zombie serve that never learned its successor, or a copied
//     credential replayed after its owner resumed, which the daemon cannot tell apart
//     and does not pretend to). A revoked one is refused. Neither is a restore.
//   - A refused or half-applied restore changes NOTHING durable. The lookup is
//     read-only, adoption happens before the generation is consumed, and the
//     conditional UPDATE that consumes it is the last step, so a degraded attempt
//     (the ID is held by a live session, the name is reserved elsewhere, the store
//     hiccups) leaves the legitimate claimant's credential valid for the retry, and
//     the fencing arbitration never mistakes a degraded attempt for a superseded one.
//
// A presented credential that matches NO retained generation of a conversation that
// has a current one counts toward revocation (three strikes). The request `_meta` is
// client-settable and conversation ids are client-visible claims, so any connection
// can present three junk credentials against a victim's conversation and revoke it:
// the accepted, bounded griefing vector of design §5. The worst case is name-only
// continuity until the victim re-establishes one, with a loud log line.

import (
	"context"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// presentation is a credential a session_start presented, once it has passed the
// gates that let it count for anything.
type presentation struct {
	linkage string
	hash    string
	proxyID string
}

// resumeWithCredential acts on a resume credential carried by the session_start call
// ctx belongs to, BEFORE the name-only relink, and reports what it did for the caller.
// "" means nothing was presented, or it was not eligible to prove anything.
func (s *connSession) resumeWithCredential(ctx context.Context, linkage string) tools.ResumeOutcome {
	p, ok := s.eligiblePresentation(ctx, linkage)
	if !ok {
		return ""
	}
	lk, err := s.sessionState.LookupResumeCredential(linkage, p.hash)
	if err != nil {
		// An unreadable credential table degrades to "no credential recorded":
		// matching fails closed and the existing resume proceeds untouched.
		s.log().Warn("daemon: could not look up a presented resume credential; falling back to a name resume", "err", err)
		return ""
	}
	switch lk.State {
	case sessionstate.CredentialCurrent:
		return s.acceptResumeCredential(ctx, p, lk)
	case sessionstate.CredentialSuperseded:
		s.log().Warn("daemon: a resume credential that was already superseded was presented — "+
			"either a replaced process that never learned its successor, or a copied credential replayed after its "+
			"owner resumed; refusing it and falling back to a name resume",
			"external_id", linkage, "presented_generation", lk.Generation, "session_id", s.sessionID())
		return tools.ResumeSuperseded
	case sessionstate.CredentialRevoked:
		s.log().Info("daemon: a revoked resume credential was presented; falling back to a name resume",
			"external_id", linkage, "presented_generation", lk.Generation)
		return tools.ResumeRevoked
	}
	s.countFailedPresentation(linkage, lk)
	return ""
}

// eligiblePresentation applies the gates in the file comment. ok is false for a call
// that presented nothing or may not prove anything by presenting.
func (s *connSession) eligiblePresentation(ctx context.Context, linkage string) (presentation, bool) {
	secret := mcp.ResumeCredentialFromCtx(ctx)
	if secret == "" || linkage == "" || !s.callerIsOwner(mcp.LogicalAgentFromCtx(ctx), linkage) {
		return presentation{}, false
	}
	v := s.view()
	if !s.namePersistEnabled(v) || v.recovery != recoveryEstablished {
		return presentation{}, false
	}
	if cur := s.externalID(); cur != "" && cur != linkage {
		return presentation{}, false
	}
	return presentation{linkage: linkage, hash: sessionstate.HashResumeSecret(secret), proxyID: v.proxySessionID}, true
}

// countFailedPresentation counts a presentation that matched nothing against a
// conversation that has a current credential, and logs the revocation it may cause.
func (s *connSession) countFailedPresentation(linkage string, lk sessionstate.CredentialLookup) {
	if !lk.HasCurrent {
		return // no record for the conversation: a re-homed machine is not an attack
	}
	counted, revoked, err := s.sessionState.NoteFailedResumePresentation(linkage)
	if err != nil {
		s.log().Warn("daemon: could not count a failed resume presentation", "err", err)
		return
	}
	if counted && revoked {
		s.log().Warn("daemon: a conversation's resume credential was revoked after repeated presentations that matched "+
			"no generation; continuity falls back to a name resume until a new credential is established",
			"external_id", linkage, "limit", sessionstate.ResumeFailureLimit, "session_id", s.sessionID())
	}
}

// acceptResumeCredential runs the full restore for a presentation that matched a
// current generation, and consumes the generation only when it succeeded.
func (s *connSession) acceptResumeCredential(ctx context.Context, p presentation, lk sessionstate.CredentialLookup) tools.ResumeOutcome {
	if lk.ProxySessionID == p.proxyID {
		return "" // this connection's own record: nothing to escalate
	}
	old, ok, err := s.sessionState.LoadIdentity(lk.ProxySessionID)
	if err != nil || !ok || old.SessionID == "" {
		s.log().Warn("daemon: a presented resume credential matched a record with no session ID to resume",
			"external_id", p.linkage, "err", err)
		return ""
	}
	if s.afterResumeLookup != nil {
		s.afterResumeLookup()
	}
	prior := s.view().persistedIdentity
	s.mutate(func(v *sessionView) { v.persistedIdentity = old })
	adoption := s.adoptStoredID(old)
	if adoption != idResumed {
		s.mutate(func(v *sessionView) { v.persistedIdentity = prior })
		return s.refusedResume(p)
	}
	if !s.restoreStoredName(old, adoption) || !s.persistIdentity() {
		// The ID came back and the name or the record did not: not a restore. Say so
		// as every degraded outcome does, and consume nothing, so a later attempt
		// retries on the same generation. No retry is scheduled: the connection's own
		// record names a stand-in, and a retry would "restore" that.
		s.setRecovery(recoveryDegraded)
		s.log().Warn("daemon: a resume credential carried the session ID back but the name or the record could not be applied; "+
			"running degraded, and the credential is not consumed", "external_id", p.linkage, "session_id", s.sessionID())
		return ""
	}
	s.rotateAfterRestore(ctx, p, lk)
	return tools.ResumeRestored
}

// refusedResume says what a refused adoption meant. The ID is held by a live session;
// if the credential has moved since the lookup, another claimant of the same secret
// won the rotation and this one is the loser — told, running under a temporary
// identity under the degraded rules, and not closed. Otherwise nothing changed: an
// ordinary overlap, and the generation is exactly where it was.
func (s *connSession) refusedResume(p presentation) tools.ResumeOutcome {
	again, err := s.sessionState.LookupResumeCredential(p.linkage, p.hash)
	if err != nil {
		return ""
	}
	switch again.State {
	case sessionstate.CredentialSuperseded:
		s.setRecovery(recoverySuperseded)
		s.log().Warn("daemon: lost the resume rotation to another claimant of the same credential; "+
			"this connection keeps running under a temporary identity",
			"external_id", p.linkage, "presented_generation", again.Generation, "session_id", s.sessionID())
		return tools.ResumeSuperseded
	case sessionstate.CredentialRevoked:
		return tools.ResumeRevoked
	}
	return ""
}

// rotateAfterRestore consumes the presented generation, issues the successor under
// this connection's proxy credential and stages it for the response. It runs after the
// identity is applied and durable, so every failure to rotate leaves a restored
// connection holding a credential that is still current: logged, never fatal.
func (s *connSession) rotateAfterRestore(ctx context.Context, p presentation, lk sessionstate.CredentialLookup) {
	s.finishRestore(p.linkage)
	succ, err := sessionstate.NewResumeSecret()
	if err != nil {
		s.log().Warn("daemon: could not generate the successor resume credential", "err", err)
		return
	}
	won, generation, err := s.sessionState.RotateResumeCredential(p.linkage, p.hash, p.proxyID, sessionstate.HashResumeSecret(succ))
	switch {
	case err != nil:
		s.log().Warn("daemon: the identity was restored but the resume credential could not be rotated; "+
			"the presented generation stays current", "external_id", p.linkage, "err", err)
	case !won:
		s.log().Warn("daemon: the identity was restored but another claimant had already consumed the presented credential",
			"external_id", p.linkage, "presented_generation", lk.Generation)
	default:
		// On THIS call's scratchpad: it is disclosed in the response of the session_start
		// that earned it, so a resume costs one round-trip and never leaves the claimant
		// holding a dead secret.
		mcp.NoteResultMeta(ctx, mcp.MetaResumeCredentialKey, succ)
		s.log().Info("daemon: a resume credential restored the full identity and rotated",
			"external_id", p.linkage, "generation", generation, "session_id", s.sessionID())
	}
}

// finishRestore records the result of a full restore on the connection: the outcome,
// the durable record it now answers to, and the shard that may have been created
// before the linkage arrived.
func (s *connSession) finishRestore(linkage string) {
	s.setRecovery(recoveryRestored)
	if rec, ok, err := s.sessionState.LoadIdentity(s.view().proxySessionID); err == nil && ok {
		s.mutate(func(v *sessionView) { v.persistedIdentity = rec })
	}
	s.seedShardOnLink(linkage)
}
