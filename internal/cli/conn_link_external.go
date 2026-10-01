package cli

// conn_link_external.go — session_start's external-ID linker, and who is told what
// resuming meant (#556).
//
// A connection has ONE linkage: the conversation its session record answers to. It
// is what `plumb mail --external-id`, the Stop-hook wake, name inheritance and
// every claim of "this connection is that conversation" resolve through, and it is
// the OWNER of the connection's identity (conn_agent_identity.go).
//
// The linker relinks. A session_start that names a different conversation moves the
// linkage to it, as it always has, and the owner moves with it. That is deliberate:
// Claude Code's /clear keeps the serve connection and starts a NEW conversation id,
// so a connection passes from one conversation to the next in the ordinary course of
// one agent's work, and holding it to the first would strand the second without the
// name its peers write to, the mail sent to it, and the wake lookup by its id.
// Whether a stamp and the id it carries agree, and who may relink a connection that
// several conversations share, is the resume credential's question (#556 items 3 and
// 4), not this linker's.
//
// What it does decide is how much a resume is worth:
//
//   - The connection takes back its conversation's NAME as soon as it is linked, by
//     whichever agent's call that happened to be: the name is the conversation's, and
//     mail addressed to it should wait for its owner, not vanish.
//   - What resuming MEANS is told to the owner alone, and once. A subagent that
//     happened to link the connection first, or another conversation sharing it, did
//     not resume anything, and being told it had is how a subagent that had never
//     existed came to report "resumed".
//   - Nothing but the name follows. The predecessor's session ID, with the threads
//     and the mail bound to it, is never granted here: the linkage is a conversation
//     id, a routing key the model can type, and only the proxy credential
//     (restoreIdentity) proves an agent is its predecessor.

import (
	"context"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tools"
)

// resumeState is a predecessor the connection found when it was linked to a
// conversation, waiting to be reported to that conversation's owner.
type resumeState struct {
	// linkage is the conversation whose predecessor this is. The news is that
	// conversation's: a connection relinked to another one drops it.
	linkage string
	// name is the name the connection now holds as a result, "" when the rename was
	// refused (a live peer holds it).
	name string
}

// linkExternalID is session_start's external-ID linker: it records the declared
// identity, links the session RECORD to its conversation, and takes back the
// conversation's name when it resumed within the grace window.
//
// Two ids are in play and they are deliberately different. The full id
// (`<conversation>` or `<conversation>/<agent>`) is what the shard machinery
// keys on, so a hook-stamped subagent gets its own pin and trackers. The
// LINKAGE is the conversation half only: `plumb mail --external-id`, the
// Stop-hook wake and name inheritance all address the conversation, and a
// subagent declaring itself must not rewrite the linkage its parent owns.
//
// ctx is the call's PER-CALL context: the stamped identity it arrived with, which
// is what tells the conversation's owner from an agent that merely reached the
// connection first.
//
// The resume runs once per linkage, when the connection is linked to it: a session
// already linked to this conversation (the parent already attached; a second
// subagent attaching) has nothing to resume, and re-running the rename against a
// name the session already holds is at best a no-op.
func (s *connSession) linkExternalID(ctx context.Context, externalID string) tools.LinkResult {
	linkage := linkageIDOf(externalID)
	alreadyLinked := linkage != "" && s.externalID() == linkage
	session.SetExternalID(s.sessionID(), linkage)
	// The linkage may arrive AFTER the shard it belongs to. session_start's
	// Execute re-pins (creating the caller's shard, which seedsConnectionReads
	// then judges against an external id this call has not written yet) before it
	// resolves linkage, so a first session_start carrying a workspace argument
	// would otherwise cache an unseeded shard and never revisit it. Seed it here
	// instead of widening the rule.
	if !alreadyLinked {
		s.seedShardOnLink(linkage)
	}
	s.recordLogicalAgentAttach(externalID)
	// Mirror the linkage into the durable identity record. Until PLAN-426 it
	// lived only in the session JSON, which is collected 24 h after the session
	// ends — so an outage longer than that lost the linkage while the identity
	// itself survived, and `plumb mail --external-id` stopped resolving a
	// session that had in fact recovered.
	s.persistIdentity()
	if !alreadyLinked {
		s.beginResume(linkage)
	}
	return s.deliverResume(mcp.LogicalAgentFromCtx(ctx), linkage)
}

// beginResume looks for the predecessor of a connection that has just been linked
// to a conversation and takes back its name. The name is resumed under a NEW
// internal session ID — this path never adopts IDs; only the proxy credential does
// that.
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
	r := &resumeState{linkage: linkage}
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
// What the owner is told is true to what happened: the name came back (or was
// refused), and nothing else did.
func (s *connSession) deliverResume(stamp, linkage string) tools.LinkResult {
	if !s.callerIsOwner(stamp, linkage) {
		return tools.LinkResult{}
	}
	r := s.takeResume(linkage)
	if r == nil {
		return tools.LinkResult{}
	}
	return tools.LinkResult{InheritedName: r.name, NewIdentity: true}
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

// takeResume claims the pending resume of the conversation linkage, once. A resume
// found for another conversation is left where it is.
func (s *connSession) takeResume(linkage string) *resumeState {
	var r *resumeState
	s.mutate(func(v *sessionView) {
		if v.pendingResume != nil && v.pendingResume.linkage == linkage {
			r, v.pendingResume = v.pendingResume, nil
		}
	})
	return r
}
