package cli

// conn_agent_shard_persist.go — a logical agent's pin on disk: the per-agent
// row under the proxy session that lets a shared connection's agent come back to
// the workspace it chose after a daemon restart (PLAN-286).
//
// Split from conn_agent_shard.go, which owns creating and resolving a shard and
// was near the file-size cap.
//
// THE RULE for this file: a row records a root the agent itself DECLARED, and
// nothing else. Only repinAgent's move, confirmShardPin's confirmation and
// attributeConnectionPin's attribution of an explicit session_start write one;
// shardFor reads a row back as the agent's own choice and ranks it above the
// connection's current pin. A shard that was merely dragged along by the
// connection's move (followConnectionShards) holds a root nobody here chose, so
// it gets no row — and loses any it had (forgetPinForAgent), because the root
// that row names is no longer where the shard sits.

import (
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// persistPinForAgent records the logical agent's pin under (proxy session,
// agent), so a shared connection's per-agent workspace survives a daemon restart
// (PLAN-286). Mirrors persistPin, scoped to the agent.
func (s *connSession) persistPinForAgent(sh *agentShard, root, language string, origin sessionstate.PinSource) {
	s.persistPinForAgentID(sh.id, root, language, origin)
}

// persistPinForAgentID is persistPinForAgent keyed on the id alone, for the
// caller that has an identity but no shard yet: an agent whose explicit
// session_start was routed to the CONNECTION because it is the only identity
// the connection has seen. Attributing that pin is what lets the shard built
// later — once a peer declares itself and the connection turns shared — restore
// the workspace the agent actually chose.
func (s *connSession) persistPinForAgentID(id, root, language string, origin sessionstate.PinSource) {
	if id == "" || origin == sessionstate.PinSourceUnknown {
		return
	}
	v := s.view()
	if s.sessionState == nil || !v.session.PersistState || v.proxySessionID == "" || root == "" {
		return
	}
	if err := s.sessionState.UpsertPinForAgent(v.proxySessionID, id, root, language, origin); err != nil {
		s.log().Debug("daemon: persist agent pin failed", "err", err)
	}
}

// forgetPinForAgent deletes the logical agent's per-agent pin row, for a shard
// that followed the connection to a root it never chose (issue #527). Deleting
// rather than rewriting is the point: with no row a restarted shard is seeded
// from the connection's pin as it stands THEN, and keeps following it, instead of
// being restored sticky at a place the connection once took it.
func (s *connSession) forgetPinForAgent(id string) {
	if id == "" {
		return
	}
	v := s.view()
	if s.sessionState == nil || !v.session.PersistState || v.proxySessionID == "" {
		return
	}
	if err := s.sessionState.DeletePinForAgent(v.proxySessionID, id); err != nil {
		s.log().Debug("daemon: forget agent pin failed", "err", err)
	}
}

// loadPinForAgent returns the pin a logical agent persisted under (proxy session,
// agent). ok=false when nothing is recorded or persistence is disabled.
func (s *connSession) loadPinForAgent(id string) (root, language string, origin sessionstate.PinSource, ok bool) {
	v := s.view()
	if s.sessionState == nil || !v.session.PersistState || v.proxySessionID == "" {
		return "", "", sessionstate.PinSourceUnknown, false
	}
	root, language, origin, ok, err := s.sessionState.LoadPinForAgent(v.proxySessionID, id)
	if err != nil || !ok || root == "" {
		return "", "", sessionstate.PinSourceUnknown, false
	}
	return root, language, origin, true
}
