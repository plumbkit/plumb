package cli

// conn_clear_handover.go — handing a connection's identity to the conversation
// Claude Code's /clear started on it (#564).
//
// A stamped session_start from a conversation the connection is not linked to no
// longer relinks it (conn_link_external.go): the newcomer is a second conversation
// until something says otherwise. /clear is that something. The SessionStart hook
// announces the new conversation id (conn_clear_markers.go), and the first call of
// that conversation to reach a connection it can take over unambiguously consumes
// the announcement and takes the connection over, exactly as a session_start relinking it always has: the linkage
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
//   - It acts only when the handover is unambiguous: the linked conversation is the
//     ONLY conversation the connection has seen (soleConversationIs). The marker
//     names the conversation that began, not the one it replaced, so on a connection
//     several conversations share (Claude desktop's Code tab) a /clear in a
//     conversation that is not the linked one would otherwise take the connection
//     from the one that holds it. There the marker is left alone and the caller is a
//     newcomer with an identity of its own, as any second conversation is.
//
// The price of the last rule is a /clear on a shared connection, which loses the
// handover even when the cleared conversation was the linked one: with a second
// conversation on the connection there is no telling the two apart. The new
// conversation then has an identity of its own, and the name and mail stay with the
// conversation the connection is linked to.

import "strings"

// has reports whether id was committed on this connection, by any channel.
func (l *logicalAgentState) has(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.seen[id]
	return ok
}

// soleConversationIs reports whether the only conversation this connection has
// seen is linkage. A subagent's stamp, `<conversation>/<agent>`, counts for its
// conversation, so a second conversation that has so far been heard only through a
// subagent is still a second conversation. The linked conversation must itself have
// been seen: a connection that has seen no one (a restart with nothing seeded)
// cannot show that the conversation it is linked to is the one /clear replaced.
func (l *logicalAgentState) soleConversationIs(linkage string) bool {
	if linkage == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen) > 0 && l.oneConversationLocked(linkage)
}

// handOverOnClear runs for every tool call, before the call's identity is
// recorded. If stamp is a conversation's main thread that this connection has not
// seen, the connection's linked conversation is the only one it has seen, and the
// daemon holds a /clear marker for stamp, the marker is consumed and the
// connection is relinked to it.
//
// Where the connection is shared by several conversations the marker is not
// consumed and nothing moves: the caller is a newcomer. A subagent stamp never
// matches: a subagent does not begin a conversation, and its parent's first call is
// still to come.
func (s *connSession) handOverOnClear(stamp string) {
	if stamp == "" || strings.Contains(stamp, "/") {
		return
	}
	clears := s.registry.conversationClears()
	if !clears.pending() || s.logicalAgents.has(stamp) {
		return
	}
	cur := s.externalID()
	if !s.logicalAgents.soleConversationIs(cur) {
		return
	}
	if !clears.take(stamp) {
		return
	}
	s.log().Info("daemon: /clear handed the connection's identity to the conversation it started",
		"from", logicalAgentLabel(cur), "to", logicalAgentLabel(stamp))
	s.relinkTo(stamp)
}
