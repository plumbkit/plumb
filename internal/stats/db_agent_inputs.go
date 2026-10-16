package stats

// db_agent_inputs.go — the context-hint consumption proxy's read: which calls a
// logical agent made soon after a hint was sent to it.
//
// A call carries the agent in logical_agent only while plumb has seen more than
// one identity on its connection (cli connSession.attributedAgent); on a
// connection one agent holds, the connection IS the agent and the column stays
// "". A Codex connection usually serves one conversation, so before this join
// every Codex hint read as unattributed, and so did a Claude Code session
// without subagents. The connection's own session_start row says which agent
// holds it: its session_id argument is the same id the hook sends with the hint.

import (
	"fmt"
	"time"
)

// agentInputsSQL selects the calls in (?1, ?2] that belong to agent ?3: its
// stamped calls on any connection, plus the unstamped calls of a connection
// the agent alone declared. "Alone" is what plumb can observe, and it fails
// closed on three counts:
//
//   - every session_start on the connection declared a session_id (one that
//     did not could be another agent arriving unannounced),
//   - all of them declared the same one, and it is ?3,
//   - the connection never recorded a stamped call (it was never observed
//     shared).
//
// Each count looks at the connection's whole recorded life, not just the
// window: a second declaration or a stamped call AFTER the window withdraws the
// connection too, because plumb cannot tell when the other agent arrived.
//
// The limit, by construction: an agent that never identifies itself, with no
// session_start and no stamp, leaves no trace on the connection, so its calls
// are credited to the agent that did declare. That is a hook-less co-tenant on a
// shared desktop connector, or a conversation a Codex /clear started on a kept
// connection before it calls session_start. The proxy can over-credit there; it
// never credits a connection two declared agents held. Namespaces are not told
// apart either: the hint ledger records none, and the stamped path never did.
//
// The declaration may come after the hint: a session's first prompt reaches the
// hook before its first plumb call, and that first turn is the hint that
// matters most. Only connections with an unstamped call in the window are
// examined (idx_tc_called_at); the CROSS JOIN keeps win as the outer loop so
// each is probed through idx_tc_session rather than scanning every
// session_start, and each declared one is probed once more for a stamp.
const agentInputsSQL = `
WITH win AS (
  SELECT DISTINCT session_id FROM tool_calls
   WHERE called_at > ?1 AND called_at <= ?2 AND logical_agent = ''
),
starts AS (
  SELECT d.session_id,
         CASE WHEN json_valid(d.input_json) THEN json_extract(d.input_json, '$.session_id') END AS declared
    FROM win CROSS JOIN tool_calls d ON d.session_id = win.session_id
   WHERE d.tool = 'session_start'
),
declared_by AS (
  SELECT session_id FROM starts
   GROUP BY session_id
  HAVING COUNT(*) = COUNT(declared)
     AND COUNT(DISTINCT declared) = 1
     AND MAX(declared) = ?3
),
held AS (
  SELECT b.session_id FROM declared_by b
   WHERE NOT EXISTS (SELECT 1 FROM tool_calls s
                      WHERE s.session_id = b.session_id AND s.logical_agent <> '')
)
SELECT c.input_json FROM tool_calls c
 WHERE c.called_at > ?1 AND c.called_at <= ?2
   AND (c.logical_agent = ?3 OR c.session_id IN (SELECT session_id FROM held))
 ORDER BY c.called_at, c.id
 LIMIT ?4`

// InputsForAgent returns the input_json of the first limit calls the logical
// agent made in (from, to], oldest first: its stamped calls, and the unstamped
// calls of a connection it alone declared (agentInputsSQL). It is the
// context-hint consumption proxy's read: did a call soon after a hint mention
// what the hint named. An agent of "" matches nothing — an unattributed row is
// no evidence about any one agent. nil-safe.
func (d *DB) InputsForAgent(agent string, from, to time.Time, limit int) ([]string, error) {
	if d == nil || agent == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := d.db.Query(agentInputsSQL, from.UnixMilli(), to.UnixMilli(), agent, limit)
	if err != nil {
		return nil, fmt.Errorf("stats: inputs for agent: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var in string
		if err := rows.Scan(&in); err != nil {
			return nil, fmt.Errorf("stats: inputs for agent: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
