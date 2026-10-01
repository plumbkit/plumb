package cli

// conn_agent_roster.go — the session row a logical agent holds of its own
// (issues #472 and #556).
//
// A connection registers exactly one session.Info, whose Folder is the
// CONNECTION's pin, and workspace_sessions builds its roster by matching
// Folder == workspace. Two things follow for an agent multiplexed over it:
//
//   - one that re-pins its shard is invisible in the workspace it is actually
//     working in, and present in one it never touched (#472); and
//   - one that did not has no address of its own at all: it was told the
//     connection's name, claimed the connection's mail, and signed its commits
//     with the connection's name, so every subagent answered as its parent (#556).
//
// So every agent but the connection's OWNER holds a row of its own, registered
// when it first needs an identity (ensureAgentRow) and kept on the root it works
// in. Its row's name and ID are what peers address, what its mail is claimed
// under, and what its commits are signed with. The owner — the conversation the
// connection is linked to — IS the connection, and holds a row only while it works
// in a workspace the connection is not pinned to (see syncAgentRoster), where that
// row is what the roster lists and so what peers address. Who the owner is, and
// which identity it answers to, lives in conn_agent_identity.go.
//
// A row deliberately carries NO ExternalID. That field is the CONVERSATION
// linkage, and both `plumb mail --external-id` (mail.go) and session.FindEnded
// match on it without filtering child rows — so a child carrying its parent's
// linkage makes the idle-agent wake hook ambiguous and lets a reconnecting
// conversation adopt the CHILD's generated name instead of its own. The agent is
// addressable by its NAME, which is what the roster prints and what leave_note
// takes, so the linkage buys nothing here and costs both.
//
// Locking: registering and retiring a row is disk I/O under the session
// directory's flock, so it never runs under sh.mu (a peer's ordinary call reaches
// boundaryPolicy, which walks the shards and blocks on that lock, and every other
// agent then waits behind one agent's disk). sh.regMu serialises it instead, and
// takes sh.mu only for the brief reads and writes inside. Never take shardsMu
// here — teardown walks shards under shardsMu and then reads each shard's mu, so
// the reverse order would invert it.

import (
	"context"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/session"
)

// syncAgentRoster brings this agent's own session row into line with the root it
// now holds. Every failure is logged and swallowed — the roster is an
// observability surface, and a session file that cannot be written must not fail
// the re-pin that was the caller's actual request.
//
// A non-owner's row is its identity, so it is registered if it has none and moved
// with the agent otherwise, wherever the agent sits — including the connection's
// root. The owner's row exists to list it in the workspace it works in: it is
// registered while the owner has moved off its connection's pin, and retired when
// it comes back, because the connection's own row already lists it there. Compared
// the way workspace_sessions compares them, so "the roster would already list me
// here" is decided by the same rule the roster uses.
//
// Takes the shard's locks itself — deliberately NOT called with sh.mu held.
func (s *connSession) syncAgentRoster(sh *agentShard, root, language string) {
	if sh == nil || root == "" || sh.id == "" {
		return
	}
	if s.ownsConnectionID(sh.id) && filepath.Clean(root) == filepath.Clean(s.workspace()) {
		sh.regMu.Lock()
		defer sh.regMu.Unlock()
		sh.mu.Lock()
		defer sh.mu.Unlock()
		s.retireAgentRoster(sh)
		return
	}
	s.registerAgentRow(sh, root, language)
}

// ensureAgentRow gives a non-owner agent the identity it was not born with: a
// session row of its own, on the root it works in. A no-op once it has one, and
// when the connection is closing (a row registered after teardown would outlive
// every connection that could retire it).
//
// Lazy on purpose. A row is a live roster entry and a name drawn from a finite
// pool, and a connection can see many short-lived subagents; only one that
// actually uses its identity — session_start, mail, a commit trailer — needs it.
func (s *connSession) ensureAgentRow(sh *agentShard) {
	sh.mu.RLock()
	have, root, language := sh.rosterID != "", sh.root, sh.language
	sh.mu.RUnlock()
	if have {
		return
	}
	s.registerAgentRow(sh, root, language)
}

// registerAgentRow registers the agent's row on root, or moves the row it already
// has there. Idempotent and safe to race: sh.regMu makes exactly one caller
// register.
func (s *connSession) registerAgentRow(sh *agentShard, root, language string) {
	sh.regMu.Lock()
	defer sh.regMu.Unlock()
	sh.mu.RLock()
	id := sh.rosterID
	sh.mu.RUnlock()
	if id != "" {
		session.Patch(id, func(info *session.Info) {
			info.Folder = root
			info.Language = language
		})
		sh.mu.Lock()
		sh.rosterFolder = root
		sh.mu.Unlock()
		return
	}
	if s.ctx != nil && s.ctx.Err() != nil {
		return // the connection is closing: no teardown is left to retire this row
	}
	info, err := session.Register(session.Info{
		ParentID: s.sessionID(),
		Folder:   root,
		Language: language,
	})
	if err != nil {
		s.log().Debug("daemon: registering the agent's roster row failed", "agent", sh.id, "root", root, "err", err)
		return
	}
	sh.mu.Lock()
	sh.rosterID, sh.rosterName, sh.rosterFolder = info.ID, info.Name, root
	sh.mu.Unlock()
	s.log().Info("daemon: logical agent registered in its own workspace roster",
		"agent", sh.id, "root", root, "row", info.ID, "name", info.Name, "parent", s.sessionID())
}

// retireAgentRoster removes the agent's row, for an owner that has returned to its
// connection's root. Idempotent.
//
// Caller holds sh.regMu and sh.mu.
func (s *connSession) retireAgentRoster(sh *agentShard) {
	if sh == nil || sh.rosterID == "" {
		return
	}
	session.Unregister(sh.rosterID)
	sh.rosterID = ""
	sh.rosterName = ""
	sh.rosterFolder = ""
}

// unregisterAgentRosters retires every agent row this connection registered, on
// teardown. Without it a connection's agents outlive it in the roster, which is
// worse than the invisibility this file exists to fix: a peer would address a
// name nobody is listening on.
func (s *connSession) unregisterAgentRosters() {
	s.shardsMu.Lock()
	shards := make([]*agentShard, 0, len(s.shards))
	for _, sh := range s.shards {
		shards = append(shards, sh)
	}
	s.shardsMu.Unlock()
	for _, sh := range shards {
		sh.regMu.Lock()
		sh.mu.Lock()
		s.retireAgentRoster(sh)
		sh.mu.Unlock()
		sh.regMu.Unlock()
	}
}

// rosterIdentity answers workspace_sessions' per-call question: which workspace
// the CALLING agent is in, and which row is its own.
//
// A caller with no row of its own returns "" for the id, which the tool reads as
// "fall back to the connection's" — the correct answer for the owner, whose
// connection row IS its row. A non-owner is given a row first, so it never reads
// the connection's row (some other agent's) as its own.
func (s *connSession) rosterIdentity(ctx context.Context) (workspace, selfID string) {
	workspace = s.workspaceFor(ctx)
	sh, owner := s.callerFor(ctx)
	if sh == nil {
		return workspace, ""
	}
	if !owner {
		s.ensureAgentRow(sh)
	}
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return workspace, sh.rosterID
}

// touchAgentRoster keeps the calling agent's own row fresh. LastSeenAt comes
// from the file's mtime and the roster renders it as "idle Nm", so a row that is
// never touched advertises an agent as idle from the moment it pinned — while it
// is working — and hands any staleness-based reaping a timestamp that never
// moves. The connection's own row is touched by onAfterTool; this is the other
// half for an agent that has a row of its own.
//
// It also follows the agent's root. A shard that merely follows its connection
// is moved by followConnectionShards and followParentShard, neither of which
// touches the row, and a non-owner's row exists wherever the agent sits.
func (s *connSession) touchAgentRoster(agentID string) {
	if agentID == "" {
		return
	}
	s.shardsMu.Lock()
	sh := s.shards[agentID]
	s.shardsMu.Unlock()
	if sh == nil {
		return
	}
	sh.mu.RLock()
	id, root, language, synced := sh.rosterID, sh.root, sh.language, sh.rosterFolder
	sh.mu.RUnlock()
	if id == "" {
		return
	}
	session.Touch(id)
	if root != "" && root != synced {
		s.syncAgentRoster(sh, root, language)
	}
}
