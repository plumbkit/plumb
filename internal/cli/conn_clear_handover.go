package cli

// conn_clear_handover.go — handing a connection's identity to the conversation
// Claude Code's /clear started on it (#564).
//
// A stamped session_start from a conversation the connection is not linked to no
// longer relinks it (conn_link_external.go): the newcomer is a second conversation
// until something says otherwise. /clear is that something. The SessionStart hook
// announces the new conversation id (conn_clear_markers.go), and the first call of
// that conversation to reach a connection consumes the announcement and takes the
// connection over, exactly as a session_start relinking it always has: the linkage
// moves, and the name, the mail and the threads, which belong to the connection's
// session and not to its linkage, are the conversation's now.
//
// Why this is safe to do on a marker alone:
//
//   - The marker is keyed by the new conversation's own id and is one-shot, so the
//     handover happens at most once and only for the call it was announced for.
//   - It acts only on the connection that call arrives on, and only on that
//     connection's own linked identity. Nothing here reaches another connection, and
//     no stamp, claim or session_id typed into a call can request it: the only input
//     is the daemon's own marker table, filled over the local control socket.
//   - A conversation this connection has already seen is not "first". Once it holds
//     an identity of its own here, a late marker must not take the connection's
//     identity from the conversation that has it.
//
// KNOWN LIMIT: the marker names the conversation that began, not the one it
// replaced. On a connection that several conversations share, a /clear in a
// conversation that is NOT the linked one still hands the connection to the new id.
// The loser is another conversation on the same connection, which is inside the
// boundary the proxy secret draws (threat-model A6), and it recovers by being given
// an identity of its own like any newcomer.

import "strings"

// has reports whether id was committed on this connection, by any channel.
func (l *logicalAgentState) has(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.seen[id]
	return ok
}

// handOverOnClear runs for every tool call, before the call's identity is
// recorded. If stamp is a conversation's main thread that this connection has not
// seen, and the daemon holds a /clear marker for it, the marker is consumed and
// the connection is relinked to it.
//
// The marker is consumed whether or not there is a linkage to move, so a
// conversation's announcement is spent by the first connection it reaches and
// cannot wait for a second one. A subagent stamp never matches: a subagent does
// not begin a conversation, and its parent's first call is still to come.
func (s *connSession) handOverOnClear(stamp string) {
	if stamp == "" || strings.Contains(stamp, "/") {
		return
	}
	clears := s.registry.conversationClears()
	if !clears.pending() || s.logicalAgents.has(stamp) {
		return
	}
	if !clears.take(stamp) {
		return
	}
	cur := s.externalID()
	if cur == "" || cur == stamp {
		return
	}
	s.log().Info("daemon: /clear handed the connection's identity to the conversation it started",
		"from", logicalAgentLabel(cur), "to", logicalAgentLabel(stamp))
	s.relinkTo(stamp)
}
