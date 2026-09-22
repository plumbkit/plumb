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

// seedLogicalAgentsFromState re-arms the shared-connection ceiling from durable
// evidence, before OnInit attaches and before any tool call arrives.
//
// logicalAgentState.seen lives for the connection's life only, so a daemon
// restart made a connection that WAS shared read as unshared: refuse admits
// every anonymous state-changing call until two agents happen to re-declare.
// The declarations recorded under this proxy session are the evidence that the
// connection was shared, so they seed the set (PLAN-440 item 2).
//
// NOT WIRED IN PRODUCTION. Wired on 2026-09-21, it locked out a client with no
// per-call identity channel (local-agent-mode-plumb) and was unwired the same
// day. Since 8d4eb640 the ceiling arms only once some caller on the connection
// has stamped a call, so re-wiring this would no longer lock such a client out —
// but re-wiring is a decision to make, not a revert.
// TestRestartDoesNotLockOutAClientThatCannotStamp pins the lockout outcome, not
// this function's wiring.
//
// Failure is silent and leaves the live behaviour unchanged: the seed can only
// ADD identities, so an unreadable store costs the early arming, never a
// wrongly-armed gate.
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
