package cli

// conn_declaration_gate.go — when a shard workspace pin cannot move, and what
// the daemon does about it.
//
// Split from conn_agent_shard.go (same package, no API change) to keep each file
// under the ~600-line cap. Three concerns live here: the two RULES that decide
// whether a seeded shard may be corrected (correctsSeededRoot) or has settled on
// its root (confirmShardPin), and the MARKER a per-agent refusal leaves behind
// (pendingDeclaration and its accessors), which is what stops a refused agent
// resolving path-bearing calls inside a root it never chose.

import (
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// pendingDeclaration is one logical agent's REFUSED workspace declaration.
//
// It exists because the refusal used to leave the agent silently seeded on a
// root it never chose: shardFor had already cached a shard from the connection's
// pin, repinAgent then declined to move it, and every later relative-path or
// implicit-repository call resolved inside someone else's project with nothing
// in the response saying so. Recording the refusal lets the boundary guard
// refuse those calls BY NAME and name the one call that clears it.
//
// sittingOn is captured at refusal time rather than read back from the shard,
// because pendingDeclMu is a leaf lock (see conn.go) and must not reach into
// shardsMu/sh.mu to describe where the agent is stuck.
type pendingDeclaration struct {
	requested string // the workspace the agent declared
	sittingOn string // the root its shard still holds — one it never chose
}

// markDeclarationRefused records that agentID's explicit declaration of
// requested was refused while its shard still sat on sittingOn.
func (s *connSession) markDeclarationRefused(agentID, requested, sittingOn string) {
	if agentID == "" || requested == "" {
		return
	}
	s.pendingDeclMu.Lock()
	defer s.pendingDeclMu.Unlock()
	if s.pendingDecl == nil {
		s.pendingDecl = make(map[string]pendingDeclaration)
	}
	s.pendingDecl[agentID] = pendingDeclaration{requested: requested, sittingOn: sittingOn}
}

// clearDeclarationRefused drops agentID's pending marker. Called wherever the
// agent's workspace question is actually SETTLED — its declaration lands (at
// either scope), or its shard is re-seeded onto the very root it asked for. A
// marker that outlived its cause would refuse calls that are safe again, so
// every settling path clears it rather than leaving it to expire.
func (s *connSession) clearDeclarationRefused(agentID string) {
	if agentID == "" {
		return
	}
	s.pendingDeclMu.Lock()
	defer s.pendingDeclMu.Unlock()
	delete(s.pendingDecl, agentID)
}

// pendingDeclarationFor returns agentID's refused declaration, if any. Lock
// order: pendingDeclMu alone, taken last everywhere (see conn.go).
func (s *connSession) pendingDeclarationFor(agentID string) (pendingDeclaration, bool) {
	if agentID == "" {
		return pendingDeclaration{}, false
	}
	s.pendingDeclMu.Lock()
	defer s.pendingDeclMu.Unlock()
	p, ok := s.pendingDecl[agentID]
	return p, ok
}

// correctsSeededRoot reports whether a re-pin from prev to root is an agent
// CORRECTING a workspace it never chose, rather than drifting off one it did.
//
// Two conditions, and both are load-bearing. The shard must not be self-pinned:
// a root the agent actually named is its own choice, and moving off one still
// takes force: true. And the two roots must be the same tree — one contains the
// other — which is the reported shape, a git worktree at .claude/worktrees/<name>
// inside its parent checkout. That containment is also why the drift was SILENT:
// the same relative path exists in both roots, so a workspace-relative call
// resolving against the wrong one returned a plausible file rather than a
// boundary error. An unrelated workspace has neither property and is refused.
//
// Called with sh.mu held, hence the plain bool rather than a shard method.
//
// Both roots are canonical by the time they reach repinAgent — Detect and
// SynthesiseRoot resolve symlinks (issue #263), and a shard's root came through
// the same lane — so the lexical prefix test in withinRoot is sound here.
func correctsSeededRoot(selfPinned bool, prev, root string) bool {
	if selfPinned {
		return false
	}
	return withinRoot(root, prev) || withinRoot(prev, root)
}

// confirmShardPin records that this agent deliberately named the root its shard
// already holds. The per-agent counterpart of attachOrRepinTo's same-root
// promotion branch: no root moves, so the read/write/undo state and the pin
// itself stand, and only the ownership facts are upgraded — the shard stops
// following the connection, and the guard above starts protecting it.
//
// Persisted as well as set, because a shard is otherwise only written down when
// repinAgent MOVES it: a choice held in memory alone would evaporate on the next
// daemon restart, when the shard re-seeds from the connection and the agent is
// back where it started.
//
// Only an explicit session_start confirms: no other origin reaches repinAgent
// with an identified caller today, and gating it here keeps that true if one
// ever does. Called with sh.mu held, so the persist runs in the documented lock
// order (sh.mu outside, s.mu innermost) exactly as the move path's does.
func (s *connSession) confirmShardPin(sh *agentShard, root, language string, origin sessionstate.PinSource) {
	if origin != sessionstate.PinSourceSessionStart || sh.selfPinned {
		return
	}
	sh.selfPinned = true
	sh.pinOrigin = origin
	s.persistPinForAgent(sh, root, language, origin)
}
