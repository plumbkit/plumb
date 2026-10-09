package cli

// conn_agent_identity.go — who a call IS, as peers address it (#556).
//
// One `plumb serve` connection can carry several agents, but it registers ONE
// session: one name, one session ID, one set of predecessor IDs whose mail it may
// still read. That identity belongs to a single conversation — the one the
// connection is linked to, the OWNER — and every other agent multiplexed over the
// connection is somebody else. The accessors the tools were handed answered for
// the connection regardless of who asked, so a subagent was told it was its
// parent, consumed its parent's mail, read its parent's threads, and signed its
// commits with its parent's name.
//
// The rule here is the whole fix: the connection's identity goes to the owner and
// to nobody else; a non-owner gets a session row of its own (conn_agent_roster.go);
// and an agent that has neither — one that could not be attributed, or whose row
// could not be written — gets NO identity, never a borrowed one.
//
// Who the owner is: the identity whose id equals the connection's external id, the
// conversation it is linked to. A conversation's main thread is stamped with that
// id by the identity hook, and a subagent of it with `<conversation>/<agent>`, so
// only the main thread matches. The linkage is read from the session file rather
// than cached, for the reason externalID gives, which costs one small file read
// per call on a shared connection and nothing on any other.

import (
	"context"
	"strings"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// ownsConnectionID reports whether id is the conversation this connection is
// linked to: the one agent whose identity the connection's name, session ID and
// inherited predecessors are.
func (s *connSession) ownsConnectionID(id string) bool {
	return id != "" && id == s.externalID()
}

// callerFor resolves the agent shard serving the call in ctx and whether that
// caller IS the connection's own identity.
//
// No shard means the connection is not shared, or the call carries no identity.
// A stamped caller on a connection nobody else is on is the connection's only
// agent, so it is the owner — with one exception. A subagent of the conversation
// the connection is linked to is never the connection, alone on it or not: after a
// restart its parent is routinely parked on the Agent tool while it is the first
// stamped caller the new connection sees, and it must not read, claim or sign as
// that parent. It gets a shard to hold the identity it is given instead. An
// unattributable caller on a SHARED connection owns nothing it can prove, and is
// nobody: not the connection's agent by default, because the default is whichever
// agent happens to be asking.
func (s *connSession) callerFor(ctx context.Context) (sh *agentShard, owner bool) {
	if sh = s.shardFor(ctx); sh == nil {
		id := mcp.LogicalAgentFromCtx(ctx)
		switch {
		case id == "":
			return nil, !s.logicalAgents.sharedWith("")
		case !s.isSubagentOfLinkage(id):
			return nil, true
		}
		sh = s.shardOf(id)
	}
	return sh, s.ownsConnectionID(sh.id)
}

// isSubagentOfLinkage reports whether id is a hook-stamped subagent of the
// conversation this connection is linked to. Only an id shaped like a subagent's
// (`<conversation>/<agent>`) pays for reading the linkage, so the main thread of a
// connection nobody shares costs no I/O here.
func (s *connSession) isSubagentOfLinkage(id string) bool {
	if !strings.Contains(id, "/") {
		return false
	}
	cur := s.externalID()
	return cur != "" && linkageIDOf(id) == cur
}

// callerIdentity is who a call is, as peers address it.
type callerIdentity struct {
	// name is what peers call it and id is what its mail is bound to; both are ""
	// for a caller with no identity of its own.
	name, id string
	// owner: the identity is the connection's own.
	owner bool
}

// identityFor resolves the caller's identity for a path about to use it, giving
// a non-owner a roster row if necessary. Preview-only paths use knownInboxFor
// without registering a new recipient.
//
// The owner is the connection's identity, unless it holds a row of its own: it
// does only while it works in a workspace the connection is not pinned to, where
// that row is what the workspace's roster lists and so what peers there address, so
// it is the identity the owner answers to there (conn_agent_roster.go).
func (s *connSession) identityFor(ctx context.Context) callerIdentity {
	sh, owner := s.callerFor(ctx)
	if sh == nil {
		if owner {
			return s.connectionIdentity()
		}
		return callerIdentity{}
	}
	if !owner {
		s.ensureAgentRow(sh)
	}
	sh.mu.RLock()
	name, id := sh.rosterName, sh.rosterID
	sh.mu.RUnlock()
	if id == "" && owner {
		return s.connectionIdentity()
	}
	return callerIdentity{name: name, id: id, owner: owner}
}

// connectionIdentity is the identity the connection registered: its owner's.
func (s *connSession) connectionIdentity() callerIdentity {
	return callerIdentity{name: s.sessionName(), id: s.sessionID(), owner: true}
}

// sessionNameFor is the calling agent's own session name: the connection's for
// the owner, its own row's otherwise, and "" for a caller that has none. Never
// another agent's name. For display; addressableNameFor is the one that routes.
func (s *connSession) sessionNameFor(ctx context.Context) string {
	return s.identityFor(ctx).name
}

// sessionIDFor is the calling agent's own session ID, on the same rule.
func (s *connSession) sessionIDFor(ctx context.Context) string {
	return s.identityFor(ctx).id
}

// addressableNameFor is the calling agent's name when peers can safely route to
// it: it holds a registered session, which every other session's uniqueness check
// can see. An unregistered one keeps a display name but no address.
func (s *connSession) addressableNameFor(ctx context.Context) string {
	who := s.identityFor(ctx)
	if who.id == "" {
		return ""
	}
	return who.name
}

// inheritedSessionIDsFor is the predecessor session IDs the calling agent may read
// mail and threads for: the connection's, for the owner, and nothing for anybody
// else. They are bound to the session a conversation had before it came back, so
// they are that conversation's main thread's alone — a subagent or another
// conversation on the same connection that held them would read, and consume, mail
// that is not theirs.
func (s *connSession) inheritedSessionIDsFor(ctx context.Context) []string {
	if _, owner := s.callerFor(ctx); owner {
		return s.inheritedSessionIDs()
	}
	return nil
}

// inboxFor is the message inbox of the calling agent in ctx, for a path about to
// claim from it. See inbox for what that is.
func (s *connSession) inboxFor(ctx context.Context) tools.Inbox {
	return s.resolvedInbox(ctx, true)
}

// knownInboxFor is the inbox of the calling agent in ctx WITHOUT giving it an
// identity it does not have yet: for the preview that rides every tool result,
// which must not register a roster row for every subagent that merely runs a
// tool. An agent with no row has no address, so nobody can have written to it.
func (s *connSession) knownInboxFor(ctx context.Context) tools.Inbox {
	return s.resolvedInbox(ctx, false)
}

// mailboxRecipient captures the recipient and root together, so a concurrent
// re-pin cannot combine one workspace's roster identity with another's store.
// A roster update in progress has no proven address until its folder catches up.
func (s *connSession) mailboxRecipient(ctx context.Context, register bool) (callerIdentity, string, []string) {
	if _, _, pending := s.pendingDeclarationForCall(ctx); pending {
		return callerIdentity{}, "", nil
	}
	sh, owner := s.callerFor(ctx)
	if register && sh != nil && !owner {
		s.ensureAgentRow(sh)
	}
	v := s.view()
	who := callerIdentity{owner: owner}
	root := v.acquiredRoot
	if sh != nil {
		sh.mu.RLock()
		root = sh.root
		if sh.rosterID != "" && sh.rosterFolder == root {
			who.name, who.id = sh.rosterName, sh.rosterID
		}
		sh.mu.RUnlock()
	}
	if owner && who.id == "" && root == v.acquiredRoot {
		who.name, who.id = v.sessName, s.sessionID()
	}
	var inherited []string
	if owner {
		inherited = v.inheritedSessionIDs
	}
	return who, root, inherited
}

// resolvedInbox builds one immutable routing snapshot per call. A config
// hot-reload takes effect on the next delivery.
//
// An unregistered session (no ID) has no address. Its name never entered the
// session directory, so no peer's uniqueness check can see it and it may well
// duplicate a live session's name — claiming that peer's messages, which are
// delivered exactly once and would simply never arrive. Inbox.Claim treats an
// empty Self as "mailbox off".
func (s *connSession) resolvedInbox(ctx context.Context, register bool) tools.Inbox {
	who, root, inherited := s.mailboxRecipient(ctx, register)
	in := tools.Inbox{
		SelfID:          who.id,
		Root:            root,
		Policy:          s.collabPolicyAt(root),
		Workspace:       func() *collab.Store { return s.collabPool.get(root) },
		Global:          s.collabGlobalIfExists,
		WorkspaceReader: func() (*collab.Store, error) { return s.collabPool.getResult(root) },
		GlobalReader:    s.collabPool.getGlobalResult,
	}
	if who.id != "" {
		in.Self = who.name
	}
	in.InheritedIDs = inherited
	return in
}
