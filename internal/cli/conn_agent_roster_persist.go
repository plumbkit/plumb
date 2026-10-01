package cli

// conn_agent_roster_persist.go — keeping an agent's own roster identity across a
// daemon restart (#526).
//
// A non-owner agent on a shared connection holds a session row of its own
// (conn_agent_roster.go), and that row is what peers address, what its mail is
// bound to and what its commits are signed with. It lived only in the session
// directory, so a restart ended it: mail bound to its old ID was stranded, its
// name went to whoever drew it next, and the agent came back as somebody new.
//
// So the row is recorded under (proxy session, agent) next to the logical-agent
// declaration, and handed back by the same two-step the connection's own identity
// gets (conn_restore.go): register a fresh row, adopt the stored session ID, then
// take the stored name. Every refusal falls back to the fresh identity and leaves
// the record alone, for the reason restoreIdentity gives.
//
// The authorisation is the connection's, unchanged. The key is the PROXY session
// ID, the secret only the serve process holds, so a different proxy that stamps
// the same `<conversation>/<agent>` finds nothing: an agent id is a string a
// model can type and selects no record. Nothing here reads the conversation
// linkage, grants any predecessor's mail to anybody, or decides who the
// connection's owner is (threat-model A6).

import (
	"errors"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// claimRosterIdentity gives a freshly registered non-owner row the identity this
// agent held under this connection's proxy session before, or records the fresh
// one when it held none. It returns the identity the agent now holds; the row on
// disk is already in step with it.
//
// Nothing happens without a proxy credential and persistence: a connection with no
// secret has no record that could be its own.
func (s *connSession) claimRosterIdentity(agentID string, fresh session.Info) session.Info {
	v := s.view()
	if !s.namePersistEnabled(v) {
		return fresh
	}
	proxyID := v.proxySessionID
	rec, found, err := s.sessionState.RosterIdentityFor(proxyID, agentID)
	if err != nil {
		// An unreadable store is not evidence that the agent is new, and writing the
		// fresh identity over an intact record is how a momentary failure becomes a
		// permanent fork.
		s.log().Warn("daemon: could not read the agent's recorded roster identity; continuing under a new one",
			"agent", agentID, "err", err)
		return fresh
	}
	if !found {
		if err := s.sessionState.RecordRosterIdentity(proxyID, agentID, fresh.Name, fresh.ID); err != nil {
			s.log().Debug("daemon: recording the agent's roster identity failed", "agent", agentID, "err", err)
		}
		return fresh
	}
	return s.restoreRosterIdentity(agentID, rec, fresh)
}

// restoreRosterIdentity applies a recorded identity to a freshly registered row:
// the ID first, because mail is bound to it and the rename keys on it, then the
// name. Either refusal keeps what was already won and leaves the record intact, so
// a restart after the conflict has cleared restores it whole.
func (s *connSession) restoreRosterIdentity(agentID string, rec sessionstate.RosterIdentity, fresh session.Info) session.Info {
	if rec.SessionID == fresh.ID {
		return fresh
	}
	adopted, err := session.Adopt(fresh.ID, rec.SessionID)
	if err != nil {
		level := s.log().Warn
		if errors.Is(err, session.ErrIDTaken) {
			// A live session holds the ID: the predecessor is still detaching, or
			// somebody else is on it. Either way it is not ours to take.
			level = s.log().Info
		}
		level("daemon: the agent's recorded session ID could not be resumed; continuing under a new identity",
			"agent", agentID, "recorded", rec.SessionID, "using", fresh.ID, "err", err)
		return fresh
	}
	if adopted.Name == rec.Name {
		return adopted
	}
	// The reservation held for this very ID does not block its own name.
	name, err := session.RenameReserved(adopted.ID, rec.Name, s.reservedNamesFor(rec.SessionID, ""))
	if err != nil {
		s.log().Info("daemon: the agent's recorded roster name is held by another session; keeping the generated name",
			"agent", agentID, "recorded", rec.Name, "using", adopted.Name, "err", err)
		return adopted
	}
	adopted.Name = name
	return adopted
}
