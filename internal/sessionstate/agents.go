package sessionstate

// agents.go — queries about the LOGICAL AGENTS multiplexed over one proxy
// session, as opposed to the session's own pins and reads in db.go.
//
// A connection's agent set is durable evidence: it is what lets a reconnecting
// daemon know a connection was shared before it restarted, and therefore keep
// the fail-closed ceiling armed from the first call rather than the second
// declaration (PLAN-440 item 2).

import (
	"fmt"
	"time"
)

// RecordLogicalAgent durably notes that logicalAgentID was observed on this
// connection, whichever channel declared it: an attach-time session_id, a
// per-call _meta stamp, or session_start's own argument.
//
// It exists because the obvious durable source — a pinned_workspace row — is
// written only when an agent's own session_start NAMED a workspace. A subagent
// that stamps its calls and inherits the connection's pin never wrote one, so
// reading pins alone made the common topology (a coordinator that named a
// workspace, a subagent that did not) come back from a restart looking
// single-agent. nil-safe; blank ids are dropped, since the connection-level
// agent is not a logical agent.
func (s *Store) RecordLogicalAgent(proxySessionID, logicalAgentID string) error {
	if s == nil || proxySessionID == "" || logicalAgentID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO logical_agent (proxy_session_id, logical_agent_id, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(proxy_session_id, logical_agent_id)
		 DO UPDATE SET updated_at=excluded.updated_at`,
		proxySessionID, logicalAgentID, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("sessionstate: record logical agent: %w", err)
	}
	return nil
}

// LogicalAgentIDsFor returns the distinct logical-agent IDs that have recorded
// a pin under proxySessionID — the durable evidence of how many agents were
// multiplexed over this connection before a restart.
//
// The connection-level agent (logical_agent_id = ”) is excluded: it is not a
// logical agent, and counting it would make a single-agent connection read as
// shared. nil-safe; an empty result means "no evidence", never "not shared".
func (s *Store) LogicalAgentIDsFor(proxySessionID string) ([]string, error) {
	if s == nil || proxySessionID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Both sources, unioned: logical_agent is the declaration record and
	// pinned_workspace the pin record, and a database written before v8 has
	// only the latter. Reading just one of them under-counts — which, for a
	// gate that fails closed, means failing OPEN.
	rows, err := s.db.Query(
		`SELECT logical_agent_id FROM logical_agent WHERE proxy_session_id=? AND logical_agent_id<>''
		 UNION
		 SELECT logical_agent_id FROM pinned_workspace WHERE proxy_session_id=? AND logical_agent_id<>''`,
		proxySessionID, proxySessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("sessionstate: list logical agents: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sessionstate: scan logical agent: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessionstate: list logical agents: %w", err)
	}
	return out, nil
}
