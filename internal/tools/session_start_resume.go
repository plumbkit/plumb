package tools

// session_start_resume.go — telling the caller what a resume credential it
// presented did for it (docs/identity-resume-credential-design.md §3 and §5).
//
// The proxy presents a stored credential in the request `_meta` of the first
// session_start that names a conversation. The daemon decides what that is worth
// (internal/cli/conn_resume_accept.go); this file only says so, in the packet,
// because an agent that is not told its credential was refused discovers it when a
// thread reply is refused, and an agent that is not told it was fully restored keeps
// treating the predecessor's mail as lost.

// ResumeOutcome is what a presented resume credential did for the call that carried
// it. The zero value means nothing was presented or there is nothing to say: a call
// that presented nothing is described exactly as before.
type ResumeOutcome string

const (
	// ResumeRestored: the credential matched its conversation's current generation, so
	// the identity came back whole (internal session ID, mail binding, thread seats)
	// and was rotated.
	ResumeRestored ResumeOutcome = "restored"
	// ResumeSuperseded: the credential's generation had already been replaced. Either
	// a replaced process that never learned its successor, another claimant that won
	// the rotation first, or a copied credential replayed. It restored nothing.
	ResumeSuperseded ResumeOutcome = "superseded"
	// ResumeRevoked: the credential was revoked, so it restored nothing.
	ResumeRevoked ResumeOutcome = "revoked"
)

// credentialNote renders the identity-block sentence for a refused credential, or ""
// for an outcome that needs none. It never names the credential or any part of it.
func (o ResumeOutcome) credentialNote() string {
	switch o {
	case ResumeSuperseded:
		return "NOTE: the resume credential you presented was already superseded, so it restored nothing: " +
			"another process resumed this conversation first, or the credential is stale. You hold the conversation's " +
			"name at most; its previous session ID, mail and threads did not follow you.\n"
	case ResumeRevoked:
		return "NOTE: the resume credential you presented has been revoked, so it restored nothing. " +
			"You hold the conversation's name at most; its previous session ID, mail and threads did not follow you.\n"
	}
	return ""
}

// supersededRecoveryNote is what every session_start says to a connection that lost
// the rotation to another claimant of the same credential: it keeps running, under a
// temporary identity, and writes nothing durable. Said on every orientation, because
// a double-open is exactly what the human needs to see immediately.
const supersededRecoveryNote = "NOTE: another process resumed this conversation with the same resume credential first, " +
	"so you are running under a temporary identity and nothing you do is recorded as the conversation's identity. " +
	"If two copies of this conversation are open, close one.\n"
