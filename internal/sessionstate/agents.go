package sessionstate

// agents.go — queries about the LOGICAL AGENTS multiplexed over one proxy
// session, as opposed to the session's own pins and reads in db.go.
//
// A connection's agent set is durable evidence: it is what lets a reconnecting
// daemon know a connection was shared before it restarted, and therefore keep
// the fail-closed ceiling armed from the first call rather than the second
// declaration (PLAN-440 item 2).

import "fmt"

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
	rows, err := s.db.Query(
		`SELECT DISTINCT logical_agent_id FROM pinned_workspace
		 WHERE proxy_session_id=? AND logical_agent_id<>''`,
		proxySessionID,
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
