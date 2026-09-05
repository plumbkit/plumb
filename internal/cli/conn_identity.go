package cli

// conn_identity.go — the session's own identity: its name, how peers address it,
// and its purpose tag. Split out of conn.go, which holds the session state, the
// copy-on-write mutation lane, and the config/subsystem accessors.
//
// The predecessor identities a session may also answer to live in
// conn_persist.go, next to the persistence that establishes them.

import "github.com/plumbkit/plumb/internal/session"

// sessionName returns the current human-readable session name. For DISPLAY —
// the TUI, daemon_info, log context, the stats row. Use addressableName for
// anything that routes messages.
func (s *connSession) sessionName() string {
	return s.view().sessName
}

// sessionID returns the current plumb session ID. It is read under sessIDMu
// because onSessionID may adopt the stable ID the serve proxy replayed
// (PLAN-296); reading fresh each call lets every consumer wired with the
// accessor see the adopted ID rather than the value captured at registration.
func (s *connSession) sessionID() string {
	s.sessIDMu.RLock()
	defer s.sessIDMu.RUnlock()
	return s.sessID
}

// setSessionID replaces the live plumb session ID. It is the ONLY writer; every
// consumer reads through sessionID. Called once, during initialize, when the
// replayed stable ID is adopted — never on the tool-call path.
func (s *connSession) setSessionID(id string) {
	s.sessIDMu.Lock()
	s.sessID = id
	s.sessIDMu.Unlock()
}

// addressableName returns the session name only when it is a name peers can
// safely route to — that is, when this session is registered in the session
// directory, where every other session's uniqueness check can see it.
//
// An unregistered session (sessID empty because session.Register failed) still
// carries a display name, but it was drawn without a uniqueness check and is
// invisible to every future one, so it may silently shadow a live peer.
func (s *connSession) addressableName() string {
	if s.sessionID() == "" {
		return ""
	}
	return s.sessionName()
}

// sessionPurpose returns the current session purpose tag ("" when unset).
func (s *connSession) sessionPurpose() string {
	return s.view().purpose
}

// setPurpose records a validated session purpose tag on the live session view
// and persists it to the session file so the TUI and workspace_sessions surface
// it. Subsequent stats rows for this session carry the tag.
func (s *connSession) setPurpose(purpose string) {
	s.mutate(func(v *sessionView) { v.purpose = purpose })
	session.SetPurpose(s.sessionID(), purpose)
}

// renameSession renames the session, persisting the new name in the session
// file and stats store, and — when per-connection persistence is on — in the
// durable identity record, so a daemon restart comes back under the same name.
//
// This is a LEGITIMATE rename: it moves the durable name reservation with the
// name and increments its revision (SaveIdentity does the increment), so a
// proxy still holding the older name cannot replay it back over this one.
// The durable name reservations are consulted alongside the live-session
// uniqueness check, so a rename cannot take a name a disconnected-but-
// recoverable session is coming back to. A session's own reservation never
// blocks it, which is what lets identity recovery restore its own retained name
// through this same path rather than needing a second, weaker one.
func (s *connSession) renameSession(name string) (string, error) {
	return s.renameSessionClaiming(name, nameClaim{})
}

// renameSessionResuming renames a session that is taking back a name on the
// strength of its external conversation ID — session_start's resume-by-linkage
// path (PR #189), where the agent is the same but the `plumb serve` process,
// and therefore every proxy-keyed credential, is not.
//
// It DOES persist: this is a real rename of a real identity, not the replay of
// a record. The next reconnect must come back under the resumed name.
func (s *connSession) renameSessionResuming(name, externalID string) (string, error) {
	return s.renameSessionClaiming(name, nameClaim{externalID: externalID})
}

// nameClaim is the identity a rename is performed ON BEHALF OF, when that is
// not simply "this connection as it currently stands".
//
// It exists because the restore path renames while the connection may still be
// running under a TEMPORARY identity — the ID adoption can be refused while the
// name restore succeeds — and the two things that follow from a rename both have
// to know that:
//
//   - which reservations to set aside (the one held for the identity being
//     restored is not a stranger's), and
//   - whether the durable record may be re-recorded at all.
//
// A zero nameClaim is an ordinary rename by a session acting as itself.
type nameClaim struct {
	// sessionID is the internal session ID the claimed identity holds, or "" for
	// an ordinary rename.
	sessionID string
	// externalID is the caller's own conversation ID, when the claim rests on
	// that rather than on the internal session ID — a restarted `plumb serve`
	// re-linking the same conversation holds neither the old secret nor the old
	// session ID, and is nonetheless the party the name is being held for.
	externalID string
	// restoring marks a rename that is APPLYING the durable record rather than
	// changing it. Such a rename must not write the record back: the record
	// already says this name, and the connection may be running under a
	// temporary ID that would silently replace the proven one.
	restoring bool
}

// renameSessionClaiming renames on behalf of claim.
//
// The persist decision is the load-bearing part. A LEGITIMATE rename must write
// the record — otherwise the next reconnect restores the name the agent just
// changed. A RESTORING rename must not, and that asymmetry is not an
// optimisation: when the ID adoption was refused, `s.sessionID()` is a temporary
// value, and recording it replaces the only proof of what to come back to. The
// refusal paths were made to leave the record alone; without this, the fork
// simply arrived through the successful rename beside them instead.
func (s *connSession) renameSessionClaiming(name string, claim nameClaim) (string, error) {
	name, err := session.RenameReserved(s.sessionID(), name, s.reservedNamesFor(claim.sessionID, claim.externalID))
	if err != nil {
		return "", err
	}
	s.mutate(func(v *sessionView) { v.sessName = name })
	s.statsStore.RenameSession(s.sessionID(), name)
	if !claim.restoring {
		s.persistIdentity()
	}
	return name, nil
}
