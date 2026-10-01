package cli

// conn_agent_shard_follow.go — which per-agent shards a move of the
// CONNECTION's pin takes with it, and the one predicate that decides it.
//
// Split from conn_agent_shard.go, which owns creating and resolving a shard and
// was over the file-size cap, so the move (followConnectionShards) and the
// report of it (connScopeCallerRoot) share followsConnectionLocked from one
// place.

// followConnectionShards re-seeds every shard that never chose a workspace of
// its own (!selfPinned) from the connection's NEW pin, after the connection
// itself moved away from prevRoot. A shard is seeded from the connection pin at
// first use — shardFor caches it BEFORE repinAgent can refuse, so one refused
// ask left the agent cached at a root whose sticky seed then refused the
// agent's next, entirely legitimate call (the exact PLAN-398 reproduction),
// while a fresh agent asking the same thing succeeded: the fresh shard seeded
// from the CURRENT pin, the stale one had not followed. Re-seeding here restores
// the invariant "a seeded shard sits where the connection sits" without
// touching shards whose agent deliberately pinned elsewhere — per-agent
// isolation means the connection's move cannot drag an agent that chose its own
// root. Runs OUTSIDE the connection mutate lane, in the documented lock order
// (shardsMu before sh.mu, s.mu innermost), so the per-tool-call hot path's lock
// pattern is unchanged; the writes mirror repinAgent's success path, held under
// one sh.mu acquisition each.
//
// A connection's FIRST pin has no previous root, and the shards that follow it
// sit at "" — one is created there by whatever resolves the caller before the
// re-pin (session_start's own orientation does) — so "sits on prevRoot" holds for
// them too, and they move to the new root like any other follower (#567). Left
// behind, the report below (connScopeCallerRoot) told the caller it worked in the
// new root while its next call resolved against nothing.
//
// A dragged shard's agent chose nothing, so no per-agent row is written for it
// and any it had is deleted: shardFor reads a row back as a root the agent
// DECLARED, ahead of the connection's pin, and persisting the drag restored the
// agent sticky, after a restart, at a place it had only been taken to (#527).
// See conn_agent_shard_persist.go.
//
// Returns the ids of the agents whose shards followed, so session_start can
// tell the caller how many other agents its connection move took with it
// (issue #517).
func (s *connSession) followConnectionShards(prevRoot string) (followed []string) {
	v := s.view()
	if v.acquiredRoot == "" || v.acquiredRoot == prevRoot {
		return nil
	}
	s.shardsMu.Lock()
	defer s.shardsMu.Unlock()
	for id, sh := range s.shards {
		// A subagent on its conversation's CHOSEN root stays with it (#513); the
		// parent's flag is read BEFORE the child is locked (never nest two mus).
		parentChose := s.parentChoseLocked(id)
		sh.mu.Lock()
		if !followsConnectionLocked(sh, parentChose) || sh.root != prevRoot {
			sh.mu.Unlock()
			continue
		}
		sh.root = v.acquiredRoot
		sh.language = v.acquiredLanguage
		sh.pinOrigin = v.pinOrigin
		sh.prov = pinProvenanceOf(&v)
		// The connection landed on the root this agent asked for, so what its
		// refusal was about is now simply true; holding the gate would refuse
		// calls that are safe again.
		if p, ok := s.pendingDeclarationFor(sh.id); ok && p.requested == sh.root {
			s.clearDeclarationRefused(sh.id)
		}
		sh.policy = s.buildAgentPolicy(sh.root, sh.language, sh.prov)
		sh.readTracker.Reset()
		sh.writeTracker.Reset()
		sh.undoStore.Reset()
		// Copy what the call below needs while the lock is still held.
		// shardsMu does not exclude repinAgent — that takes sh.mu alone — so
		// reading sh.root after the unlock would race a concurrent per-agent
		// re-pin and could rehydrate reads for a root this shard no longer has.
		// Same rule persistReadShard states: the shard's root is read under sh.mu.
		root := sh.root
		sh.mu.Unlock()
		followed = append(followed, sh.id)
		s.rehydrateReadsForAgent(sh, root)
		s.forgetPinForAgent(sh.id)
	}
	return followed
}

// followsConnectionLocked is the one answer to "does a move of the connection's
// pin take this shard with it?". followConnectionShards asks it to decide whom
// to drag, and connScopeCallerRoot asks it to report where the caller of a
// connection-scoped move resolves afterwards. The two used to decide separately
// and disagreed about a subagent on its conversation's chosen root: the move
// left it there, but the report told it that it now worked in the connection's
// new root (review of #535 merged with #533).
//
// A shard follows unless its agent chose a root — itself this life (selfPinned),
// or in an earlier one (restored from its own persisted pin) — or it is a
// subagent anchored to its conversation's chosen root, either seeded from the
// conversation's persisted pin (parentSeeded) or with the conversation's
// in-memory shard having chosen (parentChose).
//
// restored counts as chosen because a per-agent row now means exactly that: only
// an agent's own move or confirm writes one (conn_agent_shard_persist.go), so a
// restart must not turn a root the agent named into one the connection can take
// it off. Before #527 a row could also record a root the shard had merely been
// dragged to, which is why this was left unanswered, and a restored shard was
// dragged off a workspace its agent had named when it happened to sit on the
// connection's previous root — the drift a live self-pinned shard never suffers
// (issue #468).
//
// parentChose must be parentChoseLocked(sh.id), read under shardsMu BEFORE sh.mu
// is taken, so no path holds two shards' locks at once. Caller holds sh.mu.
func followsConnectionLocked(sh *agentShard, parentChose bool) bool {
	return !sh.selfPinned && !sh.restored && !sh.parentSeeded && !parentChose
}
