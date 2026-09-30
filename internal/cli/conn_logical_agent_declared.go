package cli

// conn_logical_agent_declared.go — which conversations have DECLARED
// themselves on a connection through session_start, as distinct from which
// identities have merely been observed on it (issue #513).
//
// The fail-closed ceiling (conn_logical_agent.go) asks both questions. Seen
// decides whether the connection is shared; declared decides whether a
// per-call identity on a shared connection may be served a per-agent shard.
// Split from conn_logical_agent.go by responsibility and to keep it under the
// file-size cap.

import (
	"context"
	"strings"

	"github.com/plumbkit/plumb/internal/mcp"
)

// declare commits id's linkage as declared through session_start, and reports
// the linkage it committed ("" for a blank id).
func (l *logicalAgentState) declare(id string) string {
	linkage := linkageIDOf(strings.TrimSpace(id))
	if linkage == "" {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.declared == nil {
		l.declared = make(map[string]struct{})
	}
	l.declared[linkage] = struct{}{}
	return linkage
}

// restoreDeclared commits linkages recovered from durable evidence after a
// daemon restart. Additive, like seed: a durable view is a lower bound.
func (l *logicalAgentState) restoreDeclared(linkages []string) {
	for _, linkage := range linkages {
		l.declare(linkage)
	}
}

// declareLogicalAgent commits id's linkage as declared through session_start,
// in memory and durably under the proxy session (see restoreDeclaredLinkages).
// Only session_start's success path may call it: a refused call must leave no
// declaration behind.
func (s *connSession) declareLogicalAgent(id string) {
	linkage := s.logicalAgents.declare(id)
	if linkage == "" || s.sessionState == nil {
		return
	}
	v := s.view()
	if !v.session.PersistState || v.proxySessionID == "" {
		return
	}
	if err := s.sessionState.RecordDeclaredLinkage(v.proxySessionID, linkage); err != nil {
		s.log().Debug("daemon: recording the declared linkage failed", "linkage", logicalAgentLabel(linkage), "err", err)
	}
}

// declareSessionStartCaller declares the per-call identity a SUCCESSFUL
// session_start ran under. linkExternalID already declares the session_id
// argument; this covers a client that stamps every call (a per-call _meta, or
// the hook's argument) and calls session_start without a session_id, which
// sharedIdentityRemedy sanctions. A failed session_start declares nothing — an
// agent whose re-pin was refused never attached.
func (s *connSession) declareSessionStartCaller(ctx context.Context, toolName string, isError bool) {
	if toolName != "session_start" || isError {
		return
	}
	if id := mcp.LogicalAgentFromCtx(ctx); id != "" {
		s.declareLogicalAgent(id)
	}
}

// restoreDeclaredLinkages brings back, after a daemon restart, which
// conversations had declared themselves on this connection through
// session_start — the evidence refusal needs before it admits a stamped
// state-changing call on a shared connection (issue #513).
//
// Wired, unlike seedLogicalAgentsFromState, and the difference is the direction
// each can move the gate. That seed ADDS identities to seen, which ARMS the
// ceiling and so can refuse more — it locked out a client that cannot stamp.
// This only adds to declared, which can only ADMIT more: a declared linkage
// never causes a refusal, so restoring one cannot lock anybody out. Not
// restoring was the lockout: seen refills from the per-call stamps the hook
// keeps sending, the connection is shared again at the second identity, and
// every agent that declared before the restart — none of which will call
// session_start again unprompted — would have each state-changing call refused
// until it did.
//
// The sources are the declared_linkage rows and the identity record's own
// external linkage, both keyed on the proxy session ID. That ID is the serve
// process's own 122-bit secret (see inheritSessionID), so presenting it is
// proof of being the connection that made those declarations. The identity
// record is included because Prune never reclaims it, while declared_linkage
// ages out with the TTL like every other expendable row; logical_agent is
// deliberately NOT a source — it records every OBSERVED identity, so an
// invented id that made one admitted read would come back declared.
//
// KNOWN LIMIT: a declaration older than the session-state TTL whose row a
// restart pruned, from a conversation that is not the identity record's
// linkage, comes back undeclared. Its next state-changing call is refused with
// the remedy (call session_start), which re-declares it. Persistence off
// ([session] persist_state = false) behaves the same way for every agent.
func (s *connSession) restoreDeclaredLinkages(proxySessionID string) {
	if s.sessionState == nil || !s.view().session.PersistState || proxySessionID == "" {
		return
	}
	linkages, err := s.sessionState.DeclaredLinkagesFor(proxySessionID)
	if err != nil {
		s.log().Debug("daemon: restoring declared linkages failed", "err", err)
	}
	if ext := s.view().persistedIdentity.ExternalID; ext != "" {
		linkages = append(linkages, ext)
	}
	s.logicalAgents.restoreDeclared(linkages)
}
