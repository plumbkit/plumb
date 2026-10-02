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
//   - Only on a connection whose proxy announced it consumes and strips the key
//     (conn_resume_credential.go), so the successor a resume discloses can never ride a
//     frame an old proxy forwards to its client.
//   - Only the OWNER of the connection's conversation, on a connection that is a FRESH
//     proven identity (recovery established, a proxy credential, persistence on) and
//     not linked to some other conversation. A subagent, another conversation, an
//     ordinary MCP client, a connection that is degraded or already restored: ignored,
//     and the call behaves exactly as it does today.
//   - Only a CURRENT generation restores. A superseded one is a replay (refused, and
//     logged loudly: a zombie serve that never learned its successor, or a copied
//     credential replayed after its owner resumed, which the daemon cannot tell apart
//     and does not pretend to). A revoked one is refused. Neither is a restore.
//   - The arbitration comes FIRST. The lookup is read-only; the conditional UPDATE that
//     consumes the generation is the first thing that changes anything, ahead of any
//     adoption or durable write. A claimant that loses it has applied nothing, so it has
//     nothing to undo, and it is told it was superseded however far the winner has got:
//     the old order (adopt, write, then arbitrate) let a loser that lost between the
//     winner's adoption and its rotation see "current" and be told nothing, or keep a
//     restored identity it had no right to. A claimant that wins and then cannot finish
//     (the ID is held by a live session, the name is reserved elsewhere, the store
//     hiccups) gives the generation back, so the legitimate claimant's credential is
//     valid for the retry.
//
// A presented credential that matches NO retained generation of a conversation that
// has a current one counts toward revocation (three strikes). The request `_meta` is
// client-settable and conversation ids are client-visible claims, so any connection
// can present three junk credentials against a victim's conversation and revoke it:
// the accepted, bounded griefing vector of design §5 (docs/threat-model.md, residual
// risks). The worst case is name-only continuity until the victim re-establishes one,
// with a loud log line.

import (
	"context"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// resumeSeams are test seams between the steps of an accepted resume. Both are nil in
// production.
type resumeSeams struct {
	afterLookup  func() // between a credential lookup and its claim
	afterRestore func() // between a restore and its successor being issued
}

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
	if !v.credentialConsumer || !s.namePersistEnabled(v) || v.recovery != recoveryEstablished {
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
// current generation: it claims the generation, applies the identity, and issues the
// successor, giving the generation back if the restore cannot complete.
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
	succ, err := sessionstate.NewResumeSecret()
	if err != nil {
		s.log().Warn("daemon: could not generate the successor resume credential; not resuming, and nothing is consumed", "err", err)
		return ""
	}
	if s.resumeSeams.afterLookup != nil {
		s.resumeSeams.afterLookup()
	}
	won, consumed, err := s.sessionState.ClaimResumeCredential(p.linkage, p.hash)
	switch {
	case err != nil:
		s.log().Warn("daemon: could not claim a presented resume credential; falling back to a name resume", "err", err)
		return ""
	case !won:
		return s.lostResume(p)
	}
	// From here this connection holds the only live claim on the generation, and every
	// way out below either finishes the rotation or gives the generation back.
	if !s.applyResumedIdentity(p, old) {
		s.releaseClaim(p)
		return ""
	}
	s.finishRestore(p.linkage)
	if s.resumeSeams.afterRestore != nil {
		s.resumeSeams.afterRestore()
	}
	s.issueSuccessor(ctx, p, consumed, succ)
	return tools.ResumeRestored
}

// applyResumedIdentity moves this connection onto the predecessor's session ID and
// name and records the result durably. It reports false, with the connection no
// further than a degraded or temporary one, when any part could not be applied: the
// caller then gives the claimed generation back.
func (s *connSession) applyResumedIdentity(p presentation, old sessionstate.Identity) bool {
	prior := s.view().persistedIdentity
	s.mutate(func(v *sessionView) { v.persistedIdentity = old })
	adoption := s.adoptStoredID(old)
	if adoption != idResumed {
		// The ID is held by a live session: an ordinary overlap. The arbitration has
		// already been decided in this claimant's favour, so this is not a lost race.
		s.mutate(func(v *sessionView) { v.persistedIdentity = prior })
		return false
	}
	if !s.restoreStoredName(old, adoption) || !s.persistIdentity() {
		// The ID came back and the name or the record did not: not a restore. Say so
		// as every degraded outcome does. No retry is scheduled: the connection's own
		// record names a stand-in, and a retry would "restore" that.
		s.setRecovery(recoveryDegraded)
		s.log().Warn("daemon: a resume credential carried the session ID back but the name or the record could not be applied; "+
			"running degraded, and the credential is not consumed", "external_id", p.linkage, "session_id", s.sessionID())
		return false
	}
	return true
}

// lostResume says what a lost claim meant. The conditional UPDATE did not land, so the
// generation had already moved when this claimant arrived at it: another claimant of
// the same secret won, or the credential was revoked meanwhile. This claimant applied
// nothing, wrote nothing, and is told: it keeps running under a temporary identity
// under the degraded rules, and is not closed.
func (s *connSession) lostResume(p presentation) tools.ResumeOutcome {
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

// releaseClaim gives back the generation this connection claimed for a restore that
// did not complete, so the legitimate claimant's retry presents the same credential.
func (s *connSession) releaseClaim(p presentation) {
	if _, err := s.sessionState.ReleaseResumeCredential(p.hash); err != nil {
		s.log().Warn("daemon: could not give back a resume credential claimed for a restore that did not complete; "+
			"it stays consumed until the conversation is re-established", "external_id", p.linkage, "err", err)
	}
}

// issueSuccessor records the successor under this connection's proxy credential and
// stages it for the response. It runs after the identity is applied and durable, so a
// failure here leaves a restored connection: the generation is given back (the
// restore is real, and a credential that is still current is the status quo before a
// rotation existed), logged, never fatal.
func (s *connSession) issueSuccessor(ctx context.Context, p presentation, consumed int64, succ string) {
	generation, err := s.sessionState.IssueResumeSuccessor(p.proxyID, sessionstate.HashResumeSecret(succ), consumed)
	if err != nil {
		s.log().Warn("daemon: the identity was restored but the successor resume credential could not be recorded; "+
			"the presented generation is given back", "external_id", p.linkage, "err", err)
		s.releaseClaim(p)
		return
	}
	// On THIS call's scratchpad: it is disclosed in the response of the session_start
	// that earned it, so a resume costs one round-trip and never leaves the claimant
	// holding a dead secret.
	mcp.NoteResultMeta(ctx, mcp.MetaResumeCredentialKey, succ)
	s.log().Info("daemon: a resume credential restored the full identity and rotated",
		"external_id", p.linkage, "generation", generation, "session_id", s.sessionID())
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
