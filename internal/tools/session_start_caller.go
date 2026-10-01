package tools

// session_start_caller.go — session_start answers for ONE caller (#556).
//
// A connection can carry several agents, so "who is calling" is a per-call fact
// and every identity-shaped thing session_start reports has to be derived from
// it: the Session line, the peer digest that must not list the caller as its own
// peer, the mailbox the call may claim, and what linking its session_id did.
// Those used to be read from connection-level accessors, so a subagent was told it
// was its parent, consumed its parent's mail, and was told it had "resumed".
//
// Rather than thread a ctx through every renderer (and every test that calls one),
// Execute binds the call's caller onto a private copy of the tool (forCall). The
// renderers keep their signatures and read the bound accessors.

import "context"

// UnlinkedReason says why a call's session_id was not linked to the connection.
type UnlinkedReason string

const (
	// UnlinkedOtherConversation: the connection already belongs to a different
	// conversation. A connection holds one linkage, and replacing it renames the
	// connection and strips the first conversation of its mail addressing (#564).
	UnlinkedOtherConversation UnlinkedReason = "other-conversation"
	// UnlinkedStampMismatch: the call is stamped as one conversation and its
	// session_id names another. The stamp is the stronger channel, and an id a
	// caller types for somebody else is a claim, not a declaration.
	UnlinkedStampMismatch UnlinkedReason = "stamp-mismatch"
	// UnlinkedAnonymous: the connection is shared and the call carried no
	// identity, so nothing says which agent typed the id.
	UnlinkedAnonymous UnlinkedReason = "anonymous"
)

// LinkResult is what linking a session_start call's session_id did, as it applies
// to THAT caller. The connection's own identity belongs to the conversation it is
// linked to, so every field but Unlinked is the owner's alone: a subagent or
// another conversation that happens to be first to call is told none of it.
type LinkResult struct {
	// InheritedName is the name this call resumed under, from a predecessor that
	// ended with the same external id. Owner only.
	InheritedName string
	// NewIdentity: the NAME was resumed but the predecessor's internal session ID
	// was not, so mail and threads bound to that ID do not follow the caller.
	// Owner only.
	NewIdentity bool
	// ThreadsInherited: the predecessor's session ID was granted, so its bound
	// mail and threads continue under this session. Owner only, and only to a
	// hook-stamped main thread of the conversation.
	ThreadsInherited bool
	// Unlinked is why this call's session_id was NOT linked, "" when it was (or
	// already is).
	Unlinked UnlinkedReason
}

// note renders the disclosure for a refused link, "" when the link was not
// refused. Without it the caller sees an orientation that reads as if linking
// worked, and discovers otherwise when its mail does not come.
func (l LinkResult) note() string {
	switch l.Unlinked {
	case UnlinkedOtherConversation:
		return "NOTE: this connection is already linked to a different conversation, so your session_id was not linked " +
			"and the connection's own name was left alone. Calls that carry your agent identity get a session name of their own.\n"
	case UnlinkedStampMismatch:
		return "NOTE: the session_id you passed names a different conversation than the identity stamped on this call, " +
			"so it was not linked.\n"
	case UnlinkedAnonymous:
		return "NOTE: this connection is shared and this call carried no agent identity, so your session_id was not linked: " +
			"plumb will not guess which agent typed it.\n"
	}
	return ""
}

// WithLinkage wires the caller-aware external-ID linker. fn receives the call's
// PER-CALL ctx (carrying the stamped identity, if any) and the session_id
// argument; it links the session to the conversation when the caller may, and
// reports what that did for this caller. Nil-safe. Returns the receiver for
// chaining.
//
// It replaces WithExternalID where the connection can tell its callers apart:
// the answer to "may this session_id be linked, and was this caller resumed?"
// differs between the conversation's main thread, a subagent of it, and a
// different conversation entirely.
func (t *SessionStart) WithLinkage(fn func(ctx context.Context, id string) LinkResult) *SessionStart {
	t.linkFn = fn
	return t
}

// WithCallerIdentity wires per-call resolvers for the caller's own session name
// and session ID, preferred over WithSelfIdentity and WithSelfSession. A caller
// with no identity of its own (an unattributable call on a shared connection)
// resolves to "" for both, and the orientation then says nothing about who it is
// rather than naming somebody else. Nil-safe. Returns the receiver for chaining.
func (t *SessionStart) WithCallerIdentity(name, id func(ctx context.Context) string) *SessionStart {
	t.selfNameFor = name
	t.selfIDFor = id
	return t
}

// WithMailboxFor wires the per-call mailbox snapshot, preferred over WithMailbox:
// the Inbox this call may claim from is the CALLER's — its name, its session ID,
// and only the predecessor IDs it is the owner of. Nil-safe. Returns the receiver
// for chaining.
func (t *SessionStart) WithMailboxFor(fn func(ctx context.Context) (on bool, inbox Inbox)) *SessionStart {
	t.mailboxFor = fn
	return t
}

// forCall returns a private copy of the tool whose self accessors and mailbox are
// bound to the caller in ctx, so every renderer below answers for that caller.
//
// A copy rather than state on the tool: one SessionStart serves every call on
// every connection it is registered for, concurrently. The struct holds only
// function values and plain data, so copying it is cheap and safe; keep it that
// way (a mutex added here would be copied, which go vet reports).
func (t *SessionStart) forCall(ctx context.Context) *SessionStart {
	c := *t
	if t.selfNameFor != nil {
		c.selfName = func() string { return t.selfNameFor(ctx) }
	}
	if t.selfIDFor != nil {
		c.selfSessID = func() string { return t.selfIDFor(ctx) }
	}
	if t.mailboxFor != nil {
		c.mailboxFn = func() (bool, Inbox) { return t.mailboxFor(ctx) }
	}
	return &c
}
