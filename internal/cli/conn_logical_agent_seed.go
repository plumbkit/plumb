package cli

// conn_logical_agent_seed.go — restoring what durable state says about a
// connection's agents when it re-attaches after a daemon restart: the
// fail-closed ceiling's seed (unwired), and the routing flag that is wired.
//
// Split from conn_persist.go by responsibility: that file owns the proxy
// session's identity and pin recovery, this one owns the question of how many
// logical agents were multiplexed over the connection and therefore what to do
// about it from the first call.

import (
	"strings"
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

// recentLogicalAgentIDs is the durable evidence of which agents were multiplexed
// over this proxy session lately: every declaration or pin recorded inside
// sharedConcurrencyWindow. nil when persistence is off or the store cannot say.
func (s *connSession) recentLogicalAgentIDs(proxySessionID string) []string {
	if s.sessionState == nil || !s.view().session.PersistState {
		return nil
	}
	ids, err := s.sessionState.LogicalAgentIDsFor(proxySessionID, time.Now().Add(-sharedConcurrencyWindow))
	if err != nil {
		s.log().Debug("daemon: reading the recently declared logical agents failed", "err", err)
		return nil
	}
	return ids
}

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
// day. The ceiling now arms on two declared identities whether or not anyone
// has stamped, so re-wiring this WOULD lock such a client out again right after
// a restart — re-wiring is a decision to make, not a revert.
// TestRestartDoesNotLockOutAClientThatCannotStamp pins the lockout outcome, not
// this function's wiring.
//
// Failure is silent and leaves the live behaviour unchanged: the seed can only
// ADD identities, so an unreadable store costs the early arming, never a
// wrongly-armed gate.
func (s *connSession) seedLogicalAgentsFromState(proxySessionID string) {
	ids := s.recentLogicalAgentIDs(proxySessionID)
	if len(ids) < 2 {
		return
	}
	s.logicalAgents.seed(ids)
	s.log().Info("daemon: shared connection re-armed from persisted per-agent pins",
		"agents", len(ids), "proxy_session", proxySessionID)
	s.markSharedConnectionDetected()
}

// noteConnectionWasShared records, from the same durable evidence, that this
// connection was shared before it re-attached — and does nothing else. It is the
// wired half of the seed: where seedLogicalAgentsFromState re-arms the
// anonymous-write gate, which locked out clients that cannot stamp, this only
// decides where an identified agent's calls are ROUTED (restoresShardFor), and
// an agent with no identity is not affected by it at all.
//
// It is what lets the first caller after a restart reach its own shard. The
// identities a connection has SEEN start empty, so the first stamped call —
// routinely a subagent, while its parent waits on it — reads as the only agent
// there is, and without this it ran on the connection's pin and read tracker
// (#523).
//
// Two agents recently declared together is the bar, not "a per-agent pin row
// exists": a lone agent's explicit session_start leaves such a row too
// (attributeConnectionPin), and routing it onto a shard would take its
// re-pins off the connection for good — the connection pin, language server and
// session record would stop following it.
//
// The window is six hours and each agent refreshes only its OWN row, by its own
// calls. A parent idle for longer than that while one subagent works drops out
// of the evidence; if the daemon restarts then, the subagent is the only agent
// recently declared and falls back to the connection, as it did before this.
func (s *connSession) noteConnectionWasShared(proxySessionID string) {
	if ids := s.recentLogicalAgentIDs(proxySessionID); len(ids) >= 2 {
		s.logicalAgents.priorShared.Store(true)
	}
}

// restoresShardFor reports whether id is routed to its own shard although what
// this process has seen does not yet make the connection shared: the connection
// was shared before it re-attached (noteConnectionWasShared) and id is a stamped
// agent. Nothing more is asked of the agent.
//
// In particular it need not hold a pin row. The common subagent has none: it is
// anchored to its parent's chosen root, or simply follows the connection, and
// asking for a row left exactly that agent on the connection's pin and read
// tracker, which is #523 one hop from the case a row covers. The shard shardFor
// builds for it seeds its root the way any shard's is (its parent's choice, its
// own row if it has one, else the connection's) and starts its own read
// tracker, where the connection's holds its parent's reads. The routing
// question also no longer reads the store, which it did on every call of an
// agent that had no row.
//
// Routing only. It is consulted by shardFor and by nothing that gates a call, so
// it cannot refuse anything and cannot re-arm the anonymous-write gate.
func (s *connSession) restoresShardFor(id string) bool {
	return id != "" && s.logicalAgents.priorShared.Load()
}

// seed commits identities recovered from durable state — the per-agent pins
// already persisted under this proxy session — so a connection that WAS shared
// before a daemon restart is shared again the moment it re-attaches, rather
// than from whenever two agents happen to re-declare.
//
// Without it the fail-closed ceiling had a hole exactly where it was most
// needed: after a restart, clients reconnect and start calling before they
// re-declare, and refuse admits every anonymous state-changing call until the
// second declaration lands (PLAN-440 item 2).
//
// It only ADDS, preserving the documented monotonicity of seen. A durable view
// is a lower bound on what this connection has observed, never an upper one: it
// can be pruned, partially written, or simply older than the live set, and
// letting a narrower view shrink the set would silently disarm a gate that is
// currently holding. Blank ids are dropped rather than recorded, so a row with
// an empty logical_agent_id — the connection-level agent — cannot pose as a
// second identity and make a single-agent connection read as shared.
func (l *logicalAgentState) seed(ids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		if l.seen == nil {
			l.seen = make(map[string]struct{})
		}
		l.seen[id] = struct{}{}
	}
}

// count reports how many distinct identities this connection has committed.
func (l *logicalAgentState) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen)
}
