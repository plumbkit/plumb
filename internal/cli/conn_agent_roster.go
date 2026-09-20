package cli

// conn_agent_roster.go — the session row a logical agent gets once it holds a
// root of its own (issue #472).
//
// A connection registers exactly one session.Info, whose Folder is the
// CONNECTION's pin, and workspace_sessions builds its roster by matching
// Folder == workspace. So an agent that re-pins its shard is invisible in the
// workspace it is actually working in, and present in one it never touched —
// observed on disk as folder=…/yayl with external_id=…pauta. That roster is how
// an agent discovers who else is live before touching a file, so the cost is a
// peer-awareness hint nobody receives and a name nobody can address.
//
// SCOPE, deliberately narrower than "a child Info per logical agent": a row is
// registered only while the agent's root DIFFERS from its connection's. An
// agent sharing its connection's root is already listed in the right workspace
// through the connection's own row, so registering a second one there would add
// name pressure and a duplicate roster entry to fix nothing. Making every agent
// individually addressable — the half that closes mail routing and the git
// trailer — is the follow-on, and it needs the roster dedupe and the
// name-collision work that #472's "worth scoping before building" is about.
//
// Locking: callers hold sh.mu (repinAgent does), matching persistPinForAgent,
// which already performs I/O there. Never take shardsMu here — teardown walks
// shards under shardsMu and then reads each shard's mu, so the reverse order
// would invert it.

import (
	"context"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/session"
)

// syncAgentRoster brings this agent's own session row into line with the root
// it now holds: registered when the agent has moved off its connection's pin,
// moved with it on a later re-pin, and retired when the agent comes back. Every
// failure is logged and swallowed — the roster is an observability surface, and
// a session file that cannot be written must not fail the re-pin that was the
// caller's actual request.
//
// Takes sh.mu itself — deliberately NOT called with it held. Registering a row
// flocks the session directory, and holding a shard lock across that stalls
// every agent on the connection behind one agent's disk I/O (see the defer in
// repinAgent).
func (s *connSession) syncAgentRoster(sh *agentShard, root, language string) {
	if sh == nil || root == "" {
		return
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.id == "" {
		return
	}
	// The connection's own row already covers an agent sitting on its pin.
	// Compared the way workspace_sessions compares them, so "the roster would
	// already list me here" is decided by the same rule the roster uses.
	if filepath.Clean(root) == filepath.Clean(s.workspace()) {
		s.retireAgentRoster(sh)
		return
	}
	if sh.rosterID != "" {
		session.Patch(sh.rosterID, func(info *session.Info) {
			info.Folder = root
			info.Language = language
		})
		return
	}
	// Deliberately NO ExternalID. That field is the CONVERSATION linkage, and
	// both `plumb mail --external-id` (mail.go) and session.FindEnded match on
	// it without filtering child rows — so a child carrying its parent's
	// linkage makes the idle-agent wake hook ambiguous and lets a reconnecting
	// conversation adopt the CHILD's generated name instead of its own. The
	// agent is addressable by its NAME, which is what the roster prints and
	// what leave_note takes, so the linkage buys nothing here and costs both.
	info, err := session.Register(session.Info{
		ParentID: s.sessionID(),
		Folder:   root,
		Language: language,
	})
	if err != nil {
		s.log().Debug("daemon: registering the agent's roster row failed", "agent", sh.id, "root", root, "err", err)
		return
	}
	sh.rosterID = info.ID
	sh.rosterName = info.Name
	s.log().Info("daemon: logical agent registered in its own workspace roster",
		"agent", sh.id, "root", root, "row", info.ID, "name", info.Name, "parent", s.sessionID())
}

// retireAgentRoster removes the agent's row, for an agent that has returned to
// its connection's root. Idempotent.
//
// Caller holds sh.mu.
func (s *connSession) retireAgentRoster(sh *agentShard) {
	if sh == nil || sh.rosterID == "" {
		return
	}
	session.Unregister(sh.rosterID)
	sh.rosterID = ""
	sh.rosterName = ""
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
		sh.mu.Lock()
		s.retireAgentRoster(sh)
		sh.mu.Unlock()
	}
}

// rosterIdentity answers workspace_sessions' per-call question: which workspace
// the CALLING agent is in, and which row is its own.
//
// A caller with no row of its own returns "" for the id, which the tool reads as
// "fall back to the connection's" — the correct answer rather than a missing
// one, since an agent sitting on its connection's root IS represented by the
// connection's row.
func (s *connSession) rosterIdentity(ctx context.Context) (workspace, selfID string) {
	workspace = s.workspaceFor(ctx)
	sh := s.shardFor(ctx)
	if sh == nil {
		return workspace, ""
	}
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return workspace, sh.rosterID
}

// sessionNameFor returns the session name for the calling agent in ctx. For a
// logical agent holding its own roster row, this is that agent's own name;
// otherwise it falls back to the connection's session name.
func (s *connSession) sessionNameFor(ctx context.Context) string {
	sh := s.shardFor(ctx)
	if sh != nil {
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		if sh.rosterName != "" {
			return sh.rosterName
		}
	}
	return s.sessionName()
}

// sessionIDFor returns the session ID for the calling agent in ctx. For a
// logical agent holding its own roster row, this is that agent's own row ID;
// otherwise it falls back to the connection's session ID.
func (s *connSession) sessionIDFor(ctx context.Context) string {
	sh := s.shardFor(ctx)
	if sh != nil {
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		if sh.rosterID != "" {
			return sh.rosterID
		}
	}
	return s.sessionID()
}

// addressableNameFor returns the addressable name for the calling agent in ctx.
// For a logical agent holding its own roster row, this is that agent's roster
// name (which is registered in the session directory); otherwise it falls back
// to the connection's addressable name.
func (s *connSession) addressableNameFor(ctx context.Context) string {
	sh := s.shardFor(ctx)
	if sh != nil {
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		if sh.rosterID != "" && sh.rosterName != "" {
			return sh.rosterName
		}
	}
	return s.addressableName()
}

// touchAgentRoster keeps the calling agent's own row fresh. LastSeenAt comes
// from the file's mtime and the roster renders it as "idle Nm", so a row that is
// never touched advertises an agent as idle from the moment it pinned — while it
// is working — and hands any staleness-based reaping a timestamp that never
// moves. The connection's own row is touched by onAfterTool; this is the other
// half for an agent that has a row of its own.
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
	id := sh.rosterID
	sh.mu.RUnlock()
	if id != "" {
		session.Touch(id)
	}
}
