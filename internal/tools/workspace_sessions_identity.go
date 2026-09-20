package tools

// workspace_sessions_identity.go — who is ASKING, on a connection multiplexing
// several logical agents.
//
// Split from workspace_sessions.go by responsibility: that file answers "who
// else is here and what have they touched", this one answers the prior question
// it depends on — which workspace the caller is in, and which row is the
// caller's own.

import "context"

// WithAgentIdentity wires the per-CALL answer to "which workspace am I in, and
// which row is me?", for a connection multiplexing several logical agents.
//
// Both questions were answered for the connection: the roster listed the
// connection's pin and marked "(you)" by the connection's session ID. That was
// merely incomplete until an agent could hold a row of its own (issue #472);
// now it is wrong, because the agent reads the roster of its own workspace,
// finds its own row and cannot tell it from a peer — so it treats its own
// writes as a peer's and re-reads files nobody else touched.
//
// Deliberately NOT applied to the collab and mailbox blocks below, which still
// key on the connection: mail is addressed to the connection's name, and moving
// its identity without moving the delivery paths would route a note to an
// address nothing listens on. That half is the follow-on #472 asks to be scoped.
//
// Nil-safe: unwired ⇒ both fall back to the connection accessors, which is what
// every existing caller and every single-agent connection gets. An empty string
// for either field falls back individually, so a caller that can answer one
// question and not the other is not forced to guess.
func (t *WorkspaceSessions) WithAgentIdentity(fn func(ctx context.Context) (workspace, selfID string)) *WorkspaceSessions {
	t.agentIdentityFn = fn
	return t
}

// resolveCaller returns the workspace to list and the row to mark as the
// caller's, preferring the per-call agent answer over the connection's.
func (t *WorkspaceSessions) resolveCaller(ctx context.Context) (workspace, selfID string) {
	workspace, selfID = t.workspace(), t.selfID()
	if t.agentIdentityFn == nil {
		return workspace, selfID
	}
	agentWS, agentID := t.agentIdentityFn(ctx)
	if agentWS != "" {
		workspace = agentWS
	}
	if agentID != "" {
		selfID = agentID
	}
	return workspace, selfID
}
