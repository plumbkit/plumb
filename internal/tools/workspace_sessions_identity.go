package tools

// workspace_sessions_identity.go — who is ASKING, on a connection multiplexing
// several logical agents.
//
// Split from workspace_sessions.go by responsibility: that file answers "who
// else is here and what have they touched", this one answers the prior question
// it depends on — which workspace the caller is in, and which row is the
// caller's own.

import (
	"context"

	"github.com/plumbkit/plumb/internal/collab"
)

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
// The same row is the caller's mail identity: the collab and mailbox blocks list
// the caller's own notes, sent and received, under it and under the name
// WithAgentName gives, which is also what the delivery paths claim under. They
// have to agree, or the listing reports an empty mailbox while a note is being
// handed over, or prints another agent's (#556).
//
// A caller whose row is "" is the connection's own, so it falls back to the
// connection's session ID below. An agent that has none and is not the
// connection's is given a row before it gets here.
//
// Nil-safe: unwired ⇒ both fall back to the connection accessors, which is what
// every existing caller and every single-agent connection gets. An empty string
// for either field falls back individually, so a caller that can answer one
// question and not the other is not forced to guess.
func (t *WorkspaceSessions) WithAgentIdentity(fn func(ctx context.Context) (workspace, selfID string)) *WorkspaceSessions {
	t.agentIdentityFn = fn
	return t
}

// WithAgentName wires the per-CALL session name resolver for multi-agent
// connections.
func (t *WorkspaceSessions) WithAgentName(fn func(ctx context.Context) string) *WorkspaceSessions {
	t.agentNameFn = fn
	return t
}

// WithCollabStoreFor wires a per-workspace collab store resolver for
// multi-agent connections.
func (t *WorkspaceSessions) WithCollabStoreFor(fn func(workspace string) *collab.Store) *WorkspaceSessions {
	t.collabStoreFor = fn
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

// resolveCallerName returns the session name to use for the caller. The per-call
// agent answer is final when it is wired, an empty one included: a caller with no
// address of its own must not be listed the mail addressed to the connection's,
// which belongs to another agent (#556).
func (t *WorkspaceSessions) resolveCallerName(ctx context.Context) string {
	if t.agentNameFn != nil {
		return t.agentNameFn(ctx)
	}
	if t.selfName != nil {
		return t.selfName()
	}
	return ""
}

// WithInheritedSessionsFor wires the per-CALL predecessor identities the caller
// provably continues, preferred over WithInheritedSessions. They belong to one
// agent on a shared connection, so the listing must ask per call: this block
// PRINTS the sender and body of every pending note it finds. Nil-safe. Returns
// the receiver for chaining.
func (t *WorkspaceSessions) WithInheritedSessionsFor(fn func(ctx context.Context) []string) *WorkspaceSessions {
	t.inheritedFor = fn
	return t
}

// wsCaller is who the listing is for: the roster row marked "you", which is also
// the identity the caller's mail is addressed, authored and claimed under, and the
// predecessor identities it continues.
type wsCaller struct {
	id        string
	name      string
	inherited []string
}

// resolveWSCaller resolves the workspace to list and the caller, once per call.
func (t *WorkspaceSessions) resolveWSCaller(ctx context.Context) (workspace string, c wsCaller) {
	workspace, c.id = t.resolveCaller(ctx)
	c.name = t.resolveCallerName(ctx)
	if t.inheritedFor != nil {
		c.inherited = t.inheritedFor(ctx)
	}
	return workspace, c
}
