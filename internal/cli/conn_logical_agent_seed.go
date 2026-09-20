package cli

// conn_logical_agent_seed.go — restoring the shared-connection ceiling from
// durable evidence when a connection re-attaches after a daemon restart.
//
// Split from conn_persist.go by responsibility: that file owns the proxy
// session's identity and pin recovery, this one owns the question of how many
// logical agents were multiplexed over the connection and therefore whether the
// fail-closed ceiling should be armed from the first call.

import (
	"time"
)

// sharedConcurrencyWindow is how recently two agents must both have declared
// themselves for a reconnecting connection to be treated as shared.
//
// It is a concurrency proxy, not a retention policy. Too long and a user's
// sequential conversations over one long-lived serve read as multiplexing, which
// refuses a single-agent user's writes forever; too short and a restart that
// takes a while re-admits anonymous writes on a genuinely shared connection. A
// few hours covers a working session with room to spare while keeping yesterday
// out of it.
const sharedConcurrencyWindow = 6 * time.Hour

func (s *connSession) seedLogicalAgentsFromState(proxySessionID string) {
	if s.sessionState == nil || !s.view().session.PersistState {
		return
	}
	ids, err := s.sessionState.LogicalAgentIDsFor(proxySessionID, time.Now().Add(-sharedConcurrencyWindow))
	if err != nil {
		s.log().Debug("daemon: seeding logical agents from persisted pins failed", "err", err)
		return
	}
	if len(ids) < 2 {
		return
	}
	s.logicalAgents.seed(ids)
	s.log().Info("daemon: shared connection re-armed from persisted per-agent pins",
		"agents", len(ids), "proxy_session", proxySessionID)
	s.markSharedConnectionDetected()
}
