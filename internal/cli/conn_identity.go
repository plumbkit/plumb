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
// file and stats store, and — when per-connection persistence is on — under the
// proxy session ID, so a daemon restart comes back under the same name.
func (s *connSession) renameSession(name string) (string, error) {
	name, err := session.Rename(s.sessionID(), name)
	if err != nil {
		return "", err
	}
	s.mutate(func(v *sessionView) { v.sessName = name })
	s.statsStore.RenameSession(s.sessionID(), name)
	s.persistName(name)
	return name, nil
}
