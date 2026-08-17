package cli

// conn_logical_agent.go — observation of the logical-agent identities a
// connection declares, and the shared-connection signature that keys on them.
//
// PLAN-286 (§3) promotes a stable, client-supplied logical-agent ID into the
// primary identity key. One `plumb serve` connection may multiplex several
// logical agents, and the daemon can only tell them apart — or refuse to share
// state — by the IDs they declare. The ID arrives on two channels:
// session_start's `session_id` argument (stable across reconnects, recorded at
// attach) and a per-call `tools/call._meta[MetaLogicalAgentKey]` (recorded per
// call, for clients that cannot set session_id at attach time).

import "sync"

// logicalAgentState tracks the distinct logical-agent IDs observed on one
// connection. Guarded by mu because a multiplexing client issues tool calls
// concurrently; observing a new ID must be safe to interleave with any other
// read of the set.
type logicalAgentState struct {
	mu sync.Mutex
	// seen is the set of distinct IDs declared so far. It only grows: an ID a
	// client has declared stays declared for the connection's life, so a later
	// re-check cannot un-see it and flip the shared flag back off.
	seen map[string]struct{}
}

// record notes that id was declared on this connection and reports whether the
// connection is now known to be shared (two or more distinct IDs). An empty id
// is not recorded: "" means "no trustworthy ID", not a distinct agent, and
// recording it would make every anonymous call read as a second agent.
func (l *logicalAgentState) record(id string) bool {
	if id == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = make(map[string]struct{})
	}
	l.seen[id] = struct{}{}
	return len(l.seen) > 1
}

// recordLogicalAgent records a logical-agent identity observed on this
// connection and logs the first sighting of a shared connection. It is the
// single choke point every identity channel feeds (session_id at attach, _meta
// per call), so the shared-connection detection sees one consistent view
// regardless of how the ID arrived.
func (s *connSession) recordLogicalAgent(id string) {
	if s.logicalAgents.record(id) {
		s.log().Warn("daemon: shared connection detected — multiple logical agents multiplexed over one serve; state is no longer isolated per agent", "logical_agent", id)
	}
}
