package cli

// conn_resume_credential.go — minting and disclosing a connection's resume
// credential (docs/identity-resume-credential-design.md §3, "Establishment and
// disclosure").
//
// A resume credential proves the same fact the proxy session ID proves — "I am the
// process entrusted with this conversation" — but outlives the serve process that
// first held it, so a replacement serve that presents it can resume the full identity
// (internal session ID, mail binding, thread seats) and not only the name. See
// conn_resume_accept.go for the other half.
//
// Two rules matter here. The first: the daemon deals in a resume credential ONLY with
// a proxy that announced it consumes and strips the key (mcp.MetaResumeCredentialConsumerKey).
// A proxy forwards every daemon frame to its client verbatim, and Claude Code persists
// a tool result's `_meta` in its on-disk transcripts, where a model with file tools can
// read it. So a connection that made no announcement is minted nothing, disclosed
// nothing and accepts no presentation: the feature is inert for it, and for every
// proxy that predates the strip. Every secret the daemon ever discloses originates in
// mintResumeCredential or in an accepted presentation (eligiblePresentation), and
// both check the announcement, so nothing downstream needs to.
//
// The second: ONLY a proven branch mints. A connection whose recovery is established
// or restored, under a proxy credential, with persistence on, is provably the recorded
// identity. A degraded connection runs under a stand-in, and issuing it a credential
// would hand the identity-fork bug a credential of its own; an ordinary MCP client or
// a session with persistence off has no continuity to offer. Each of those is told
// nothing, exactly as before.
//
// Disclosure is one hop across `_meta` and never into text: the initialize result for
// a connection that was proven at initialize, and the next successful tool result for
// one that converged later on the bounded retry (C3), which has no initialize left to
// ride. The plaintext lives in memory only until it is disclosed; the store keeps its
// SHA-256.

import (
	"context"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// recoverySuperseded: this connection presented a resume credential and lost the
// rotation to another claimant of the same one. It keeps running, TOLD and not
// closed, under a temporary identity and the standing degraded rules: it never
// converges and it never writes the durable record.
const recoverySuperseded recoveryOutcome = "superseded"

// blocksDurableWrites reports whether a connection in this outcome must not write the
// durable identity record: it is running under a stand-in, and recording a stand-in
// over the proven identity is the fork.
func (o recoveryOutcome) blocksDurableWrites() bool {
	return o == recoveryDegraded || o == recoverySuperseded
}

// mintResumeCredential issues this connection a resume credential and stages it for
// disclosure, when — and only when — its proxy consumes the key and its identity is
// proven.
//
// Called from the initialize param hook once identity has settled, and from the C3
// retry's convergence points. Failures are logged and swallowed: a connection that
// cannot be issued a credential is simply credential-less, which is the status quo,
// and the identity restore must never depend on it.
func (s *connSession) mintResumeCredential() {
	v := s.view()
	if !v.credentialConsumer {
		return // the proxy did not announce it strips the key: nothing could safely carry it
	}
	if !s.namePersistEnabled(v) {
		return // no proxy credential, or persistence off: no continuity on offer
	}
	if v.recovery != recoveryEstablished && v.recovery != recoveryRestored {
		return // degraded (or unavailable): never a credential
	}
	secret, err := sessionstate.NewResumeSecret()
	if err != nil {
		s.log().Warn("daemon: could not generate a resume credential", "err", err)
		return
	}
	if _, err := s.sessionState.MintResumeCredential(v.proxySessionID, sessionstate.HashResumeSecret(secret)); err != nil {
		s.log().Warn("daemon: could not record a resume credential; this connection has none", "err", err)
		return
	}
	s.mutate(func(v *sessionView) { v.pendingCredential = secret })
}

// takePendingCredential hands over the minted credential that has not yet been
// disclosed, exactly once. "" when there is none.
func (s *connSession) takePendingCredential() string {
	var secret string
	s.mutate(func(v *sessionView) { secret, v.pendingCredential = v.pendingCredential, "" })
	return secret
}

// nilIfEmpty is the "nothing to say" contract of toolResultMeta: an empty `_meta` is
// reported as none at all, never as an empty map.
func nilIfEmpty(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// resumeCredentialMeta is the `_meta` a tool result carries for the resume credential,
// or nil when none is owed, so a tool that has nothing to disclose reports exactly
// what it did before.
//
// Two sources, deliberately different. The successor an accepted resume produced is
// noted on THIS call's scratchpad (conn_resume_accept.go), so it reaches the response
// of the session_start that earned it and no other. A credential minted when a
// degraded connection converged has no call to belong to, so it rides the next
// successful result, once.
func (s *connSession) resumeCredentialMeta(ctx context.Context) map[string]any {
	if secret, ok := mcp.ResultMetaNote(ctx, mcp.MetaResumeCredentialKey); ok && secret != "" {
		return map[string]any{mcp.MetaResumeCredentialKey: secret}
	}
	if secret := s.takePendingCredential(); secret != "" {
		return map[string]any{mcp.MetaResumeCredentialKey: secret}
	}
	return nil
}
