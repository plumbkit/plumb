package cli

// conn_link_external.go — session_start's external-ID linker, and the predecessor
// inheritance that rides on it (#556, #564).
//
// A connection has ONE linkage: the conversation its session record answers to.
// It is what `plumb mail --external-id`, the Stop-hook wake, name inheritance and
// every claim of "this connection is that conversation" resolve through, and a
// connection that several conversations share (Claude desktop's Code tab runs
// every conversation through one server) can hold only one. The linker used to
// let the last session_start win: a second conversation replaced the linkage,
// renamed the connection to the name that conversation had held before, and left
// the first conversation without a name or a mail address and without being told.
//
// The rules it follows now:
//
//   - The FIRST conversation to link a connection owns it. A later session_start
//     naming a different conversation links nothing and renames nothing; its caller
//     is told so, and (when it carries an identity) is declared and given a session
//     of its own by conn_agent_identity.go.
//   - A call that carries no identity never replaces a linkage, and links a
//     connection only when nothing else is on it. Nothing says which agent typed
//     the id.
//   - A call stamped as one conversation cannot link another's id.
//   - The connection takes back its conversation's NAME as soon as it is linked, by
//     whichever agent's call that happened to be — the name is the conversation's,
//     and mail addressed to it should wait for its owner, not vanish. But what
//     resuming MEANS is told to the owner alone, and the predecessor's mail and
//     threads go to the owner alone.

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tools"
)

// resumeState is a predecessor the connection found when it was first linked,
// waiting to be reported to — and inherited by — the conversation's owner.
type resumeState struct {
	// predecessorID is the internal session ID of the ended session whose external
	// id matched, the identity the owner's inherited mail and threads are bound to.
	predecessorID string
	// name is the name the connection now holds as a result, "" when the rename was
	// refused (a live peer holds it).
	name string
}

// linkPlan is what a session_start session_id may do to the connection.
type linkPlan struct {
	// declare records the id as a declared identity: evidence that another agent is
	// on the connection, which is what arms the shared-connection write gate
	// (#513), and what admits the agent's later calls once it is armed.
	declare bool
	// link writes the conversation as the connection's linkage: only ever the first.
	link bool
	// reason is why the id was not linked, "" when it was or already is.
	reason tools.UnlinkedReason
}

// planLink decides what a call stamped as stamp, naming conversation linkage, may
// do. The decision is separate from its effects so each rule above is one case of
// one switch.
//
// Declaring and linking are different commitments, and only linking is refused to
// a conversation that does not own the connection. A second conversation's
// session_id is still evidence that the connection is shared, whoever typed it and
// whether or not the call was stamped: forgetting it would leave the connection
// looking like one agent's, and the anonymous writes the gate exists to refuse
// would be admitted into another conversation's checkout (the 2026-09-30
// incident). The one id declared by nobody is a stamp contradicting its own
// session_id, which names nothing the call is.
func (s *connSession) planLink(stamp, linkage string) linkPlan {
	cur := s.externalID()
	switch {
	case stamp != "" && linkageIDOf(stamp) != linkage:
		return linkPlan{reason: tools.UnlinkedStampMismatch}
	case cur == linkage:
		return linkPlan{declare: true}
	case cur != "":
		return linkPlan{declare: true, reason: tools.UnlinkedOtherConversation}
	case stamp == "" && s.logicalAgents.sharedWith(""):
		return linkPlan{declare: true, reason: tools.UnlinkedAnonymous}
	}
	return linkPlan{declare: true, link: true}
}

// linkExternalID is session_start's external-ID linker: it records the declared
// identity, links the session RECORD to its conversation when the caller may, and
// takes back the conversation's name when it resumed within the grace window.
//
// Two ids are in play and they are deliberately different. The full id
// (`<conversation>` or `<conversation>/<agent>`) is what the shard machinery keys
// on, so a hook-stamped subagent gets its own pin and trackers. The LINKAGE is the
// conversation half only: `plumb mail --external-id`, the Stop-hook wake and name
// inheritance all address the conversation, and a subagent declaring itself must
// not rewrite the linkage its parent owns.
//
// ctx is the call's PER-CALL context: the stamped identity it arrived with, which
// is what tells a hook-vouched caller from one that merely typed an id.
//
// The resume runs once per connection, at its first link: a session already
// linked to this conversation (the parent already attached; a second subagent
// attaching) has nothing to resume, and re-running the rename against a name the
// session already holds is at best a no-op.
func (s *connSession) linkExternalID(ctx context.Context, externalID string) tools.LinkResult {
	linkage := linkageIDOf(externalID)
	if linkage == "" {
		return tools.LinkResult{}
	}
	stamp := mcp.LogicalAgentFromCtx(ctx)
	plan := s.planLink(stamp, linkage)
	if plan.link {
		session.SetExternalID(s.sessionID(), linkage)
		// The linkage may arrive AFTER the shard it belongs to. session_start's
		// Execute re-pins (creating the caller's shard, which seedsConnectionReads
		// then judges against an external id this call has not written yet) before
		// it resolves linkage, so a first session_start carrying a workspace
		// argument would otherwise cache an unseeded shard and never revisit it.
		// Seed it here instead of widening the rule.
		s.seedShardOnLink(linkage)
	}
	if plan.declare {
		s.recordLogicalAgentAttach(externalID)
	}
	if plan.reason != "" {
		return tools.LinkResult{Unlinked: plan.reason}
	}
	// Mirror the linkage into the durable identity record. Until PLAN-426 it
	// lived only in the session JSON, which is collected 24 h after the session
	// ends — so an outage longer than that lost the linkage while the identity
	// itself survived, and `plumb mail --external-id` stopped resolving a
	// session that had in fact recovered.
	s.persistIdentity()
	if plan.link {
		s.beginResume(linkage)
	}
	return s.deliverResume(stamp, linkage)
}

// beginResume looks for the predecessor of a connection that has just been linked
// and takes back its name. The name is resumed under a NEW internal session ID —
// this path never adopts IDs; only the proxy credential does that.
//
// session.Rename refuses a name a live session already holds, so two resumes
// racing on one external ID inside the grace window cannot both inherit it —
// mailbox delivery matches on the name string, and an ambiguous address silently
// misdelivers. Resuming, not renaming: the entitlement is the external ID the
// caller just presented, which is what lets a RESTARTED `plumb serve` — new proxy
// secret, new session ID, same conversation — take back the name its own durable
// record reserves.
func (s *connSession) beginResume(linkage string) {
	prev := session.FindEnded(linkage, 24*time.Hour)
	if prev == nil {
		return
	}
	r := &resumeState{predecessorID: prev.ID}
	name, err := s.renameSessionResuming(prev.Name, linkage)
	if err == nil {
		r.name = name
	} else {
		// Log it: a silently dropped inheritance looks to the caller like the
		// session_id argument did nothing at all.
		s.log().Debug("daemon: could not inherit the previous session name; keeping the generated one",
			"inherited", prev.Name, "err", err)
	}
	s.mutate(func(v *sessionView) { v.pendingResume = r })
}

// deliverResume hands a pending resume to the conversation's owner, and to no
// one else. A subagent that happened to link the connection first, or another
// conversation sharing it, is told nothing: it did not resume, and being told it
// had is how a subagent that had never existed came to report "resumed" (#556).
//
// Only a hook-stamped MAIN thread of the conversation also inherits the
// predecessor's session ID, and with it the threads and mail bound to it.
func (s *connSession) deliverResume(stamp, linkage string) tools.LinkResult {
	if s.view().pendingResume == nil || !s.callerIsOwner(stamp, linkage) {
		return tools.LinkResult{}
	}
	r := s.takeResume()
	if r == nil {
		return tools.LinkResult{}
	}
	res := tools.LinkResult{InheritedName: r.name}
	if isMainThread(stamp, linkage) {
		s.inheritSessionID(r.predecessorID)
		res.ThreadsInherited = true
		return res
	}
	res.NewIdentity = true
	return res
}

// callerIsOwner reports whether a call stamped as stamp is the owner of the
// conversation linkage: its main thread, or — with no stamp to say otherwise —
// the only agent a connection nobody else is on can have.
func (s *connSession) callerIsOwner(stamp, linkage string) bool {
	if stamp != "" {
		return stamp == linkage
	}
	return !s.logicalAgents.sharedWith("")
}

// isMainThread reports whether the call is stamped as the main thread of the
// conversation linkage — the one caller entitled to inherit a predecessor's
// session ID. Three conditions, each load-bearing: it carries an identity (an
// unstamped id is a claim nothing vouches for), that identity IS the
// conversation (not another's), and it is not a subagent of it (`<conv>/<agent>`,
// which a parked parent's mail must not follow).
func isMainThread(stamp, linkage string) bool {
	return stamp != "" && stamp == linkage && !strings.Contains(stamp, "/")
}

// takeResume claims the pending resume, once.
func (s *connSession) takeResume() *resumeState {
	var r *resumeState
	s.mutate(func(v *sessionView) { r, v.pendingResume = v.pendingResume, nil })
	return r
}

// inheritSessionID accepts a predecessor's plumb session ID as an additional
// mailbox identity for this connection's OWNER, so messages and threads BOUND to
// the session a restart or reconnect ended still reach the agent they were written
// for. Without it, binding a message to a session — which is what stops a
// name-reuser reading it — would also strand every unread message and every open
// thread across one, since the reconnected connection registers under a fresh
// session ID.
//
// It is granted from exactly two places, and the authority is different in each:
//
//   - The proxy credential (restoreIdentity). The proxy session ID is a 122-bit
//     random value the serve process generates for itself, replays only inside its
//     own initialize handshake, and which plumb never writes to a session file, a
//     log line, or any tool result. Presenting it is evidence of being the same
//     serve process; being called "alice" is not. It is granted whenever the
//     credential authenticated the record, whether or not the name came back too:
//     the grant authorises a session to read ITS predecessor's mail, and a name a
//     live peer happens to hold says nothing about that.
//   - The conversation's main thread (deliverResume). Weaker, because the entitlement
//     is a conversation id rather than a secret, and so narrowed three ways: the
//     predecessor must have ended with that same external id; the caller must carry
//     the hook's stamp for that conversation; and it must be the conversation's main
//     thread, never a subagent of it or another conversation sharing the connection.
//
// Inheriting on the strength of a NAME would hand any session its predecessor's
// mailbox for the cost of one rename_session, which is precisely the hole the
// binding closed. The grant is additive and deduplicated, so the two places can
// both contribute and a retry cannot grow it. Every identity in it is the OWNER's:
// inheritedSessionIDsFor hands it to no other agent.
func (s *connSession) inheritSessionID(prevID string) {
	if prevID == "" || prevID == s.sessionID() {
		return
	}
	s.mutate(func(v *sessionView) {
		if !slices.Contains(v.inheritedSessionIDs, prevID) {
			v.inheritedSessionIDs = append(slices.Clone(v.inheritedSessionIDs), prevID)
		}
	})
	s.log().Debug("daemon: inherited predecessor mailbox identity", "predecessor", prevID)
}

// inheritedSessionIDs returns the predecessor identities the connection's owner
// may also read mail for. Nil for every session that was granted none, which is
// the overwhelming majority.
func (s *connSession) inheritedSessionIDs() []string {
	return s.view().inheritedSessionIDs
}
