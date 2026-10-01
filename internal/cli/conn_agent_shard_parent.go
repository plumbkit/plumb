package cli

// conn_agent_shard_parent.go — what a hook-stamped subagent `<conv>/<agent>`
// inherits from its conversation `<conv>` (issue #513 review): the root its
// conversation chose, and the conversation's refused declaration.
//
// A subagent rarely calls session_start; it is admitted on its conversation's
// declaration, so the workspace it resolves against must be its conversation's
// too. Split from conn_agent_shard.go to keep that file under the size cap.

import (
	"context"

	"github.com/plumbkit/plumb/internal/mcp"
)

// seedFromParentLocked seeds a NEW subagent shard sh from its conversation's
// root, when the conversation chose one: its in-memory shard is self-pinned or
// restored from its own persisted pin, or, when that shard is not in memory
// (after a daemon restart, before the parent's next call), its persisted
// per-agent pin still verifies. Otherwise sh keeps the connection seed, which is
// then also the conversation's root. A no-op for an id that is its own linkage.
//
// Caller holds s.shardsMu; the parent's mu is taken beneath it, which is the
// documented lock order (shardsMu before sh.mu).
func (s *connSession) seedFromParentLocked(sh *agentShard) {
	linkage := linkageIDOf(sh.id)
	if linkage == sh.id || linkage == "" {
		return
	}
	if parent, ok := s.shards[linkage]; ok {
		parent.mu.RLock()
		chose := parent.selfPinned || parent.restored
		root, language, origin, prov := parent.root, parent.language, parent.pinOrigin, parent.prov
		parent.mu.RUnlock()
		if chose {
			// No parentSeeded here: a parent in memory that chose keeps saying
			// so (selfPinned and restored are never cleared), and
			// followConnectionShards asks it directly.
			sh.root, sh.language, sh.pinOrigin, sh.prov = root, language, origin, prov
		}
		return
	}
	root, language, origin, ok := s.loadPinForAgent(linkage)
	if !ok {
		return
	}
	if resolved, _, intact := s.restoreRootIntact(root); intact {
		sh.root, sh.language, sh.pinOrigin = resolved, language, origin
		sh.prov = restoredProvenance(origin)
		// The parent is not in memory to be asked, so the child remembers.
		sh.parentSeeded = true
	}
}

// followParentShard re-seeds, after conversation parentID moved its own shard
// off prevRoot, every subagent `<parentID>/*` shard that never chose or
// restored a root of its own, wherever it currently sits. Seeding at creation alone left a subagent
// that had made any call before its parent re-pinned (a background subagent, a
// continued one) on the old root — another conversation's checkout — for good.
// The counterpart of followConnectionShards, for the parent's move.
//
// Lock order: shardsMu, then the parent's mu (read and RELEASED), then each
// child's mu in turn. No shard's mu is held while another's is taken. Called
// from repinAgent's post-unlock defer, so the parent's mu is not held here.
func (s *connSession) followParentShard(parentID, prevRoot string) {
	if prevRoot == "" || linkageIDOf(parentID) != parentID {
		return
	}
	s.shardsMu.Lock()
	defer s.shardsMu.Unlock()
	parent, ok := s.shards[parentID]
	if !ok {
		return
	}
	parent.mu.RLock()
	root, language, origin, prov := parent.root, parent.language, parent.pinOrigin, parent.prov
	parent.mu.RUnlock()
	for id, sh := range s.shards {
		if id == parentID || linkageIDOf(id) != parentID {
			continue
		}
		sh.mu.Lock()
		// No root comparison: a subagent that never chose a root always follows
		// its conversation. Comparing against prevRoot was NOT equivalent —
		// a concurrent connection move could drag the subagent off prevRoot
		// between the parent's move and this follow (followConnectionShards
		// read parentChose before the parent committed), and the check would
		// then strand it on the connection's new root (review of #535).
		if sh.selfPinned || sh.restored {
			sh.mu.Unlock()
			continue
		}
		sh.root, sh.language, sh.pinOrigin, sh.prov = root, language, origin, prov
		sh.policy = s.buildAgentPolicy(root, language, prov)
		sh.readTracker.Reset()
		sh.writeTracker.Reset()
		sh.undoStore.Reset()
		sh.mu.Unlock()
		s.rehydrateReadsForAgent(sh, root)
	}
}

// parentChoseLocked reports whether id is a subagent whose conversation's
// in-memory shard chose or restored its root. Caller holds shardsMu and NO
// shard's mu; the parent's mu is taken and released here.
func (s *connSession) parentChoseLocked(id string) bool {
	linkage := linkageIDOf(id)
	if linkage == id {
		return false
	}
	parent, ok := s.shards[linkage]
	if !ok {
		return false
	}
	parent.mu.RLock()
	defer parent.mu.RUnlock()
	return parent.selfPinned || parent.restored
}

// pendingDeclarationForCall is pendingDeclarationFor as a CALL sees it: the
// agent's own refused declaration, or else — for a subagent that has not chosen
// a root of its own — its conversation's. Without the fallback a subagent of a
// refused parent sat on the connection seed, the very root its conversation had
// just been refused off, and resolved relative paths and git's default
// repository inside it. The second result reports that the marker is the
// conversation's, so the refusal can say so.
func (s *connSession) pendingDeclarationForCall(ctx context.Context) (p pendingDeclaration, inherited, ok bool) {
	id := mcp.LogicalAgentFromCtx(ctx)
	if p, ok := s.pendingDeclarationFor(id); ok {
		return p, false, true
	}
	linkage := linkageIDOf(id)
	if linkage == id {
		return pendingDeclaration{}, false, false
	}
	p, ok = s.pendingDeclarationFor(linkage)
	if !ok || s.agentChoseRoot(id) {
		return pendingDeclaration{}, false, false
	}
	return p, true, true
}

// agentChoseRoot reports whether id's shard holds a root the agent chose itself
// (self-pinned, or restored from its own persisted pin). Takes shardsMu then the
// shard's mu, the documented order; callers hold neither.
func (s *connSession) agentChoseRoot(id string) bool {
	s.shardsMu.Lock()
	sh, ok := s.shards[id]
	s.shardsMu.Unlock()
	if !ok {
		return false
	}
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.selfPinned || sh.restored
}
