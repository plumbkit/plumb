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
	"slices"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// declare commits id's linkage as declared through session_start, and reports
// the linkage it committed ("" for a blank id). An existing entry keeps its
// refresh time.
func (l *logicalAgentState) declare(id string) string {
	linkage := linkageIDOf(strings.TrimSpace(id))
	if linkage == "" {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.declared == nil {
		l.declared = make(map[string]time.Time)
	}
	if _, ok := l.declared[linkage]; !ok {
		l.declared[linkage] = time.Time{}
	}
	return linkage
}

// restoreDeclared commits linkages recovered from durable evidence after a
// daemon restart. Additive, like seed: a durable view is a lower bound.
func (l *logicalAgentState) restoreDeclared(linkages []string) {
	for _, linkage := range linkages {
		l.declare(linkage)
	}
}

// oneConversationLocked reports whether every identity observed on the
// connection answers to linkage. Caller holds l.mu.
func (l *logicalAgentState) oneConversationLocked(linkage string) bool {
	for id := range l.seen {
		if linkageIDOf(id) != linkage {
			return false
		}
	}
	return true
}

// markRefreshed records that linkage's durable row was written at now.
func (l *logicalAgentState) markRefreshed(linkage string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.declared[linkage]; ok {
		l.declared[linkage] = now
	}
}

// refreshDue reports whether linkage is declared and its durable row was last
// refreshed at least every ago, and if so claims the refresh (stamps now), so
// concurrent calls do not all write. It also returns the stamp it replaced, so
// a failed write can hand the slot back (releaseRefresh). An undeclared linkage
// is never due.
func (l *logicalAgentState) refreshDue(linkage string, now time.Time, every time.Duration) (prev time.Time, due bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	last, ok := l.declared[linkage]
	if !ok || now.Sub(last) < every {
		return time.Time{}, false
	}
	l.declared[linkage] = now
	return last, true
}

// releaseRefresh hands back a refresh slot claimed at claimed, restoring prev,
// so a write that failed is retried on the next call rather than an interval
// later. A no-op if another call has claimed the slot since.
func (l *logicalAgentState) releaseRefresh(linkage string, claimed, prev time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.declared[linkage]; ok && cur.Equal(claimed) {
		l.declared[linkage] = prev
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
		return
	}
	s.logicalAgents.markRefreshed(linkage, time.Now())
}

// refreshDeclaration keeps a working conversation's durable declaration young.
//
// A declared_linkage row is otherwise written only by session_start. The idle
// reaper reclaims a row older than [session] persist_state_ttl_minutes (24 h by
// default) unless its proxy session is connected at that pass, and nothing is
// pruned at daemon start (#525). So a connected serve keeps its declarations
// however old, across restarts it reconnects through. The exemption does not
// cover a serve that is between connections when a pass runs, such as one
// reconnecting after its transport dropped, and there a conversation that
// declared once and then worked for a day would lose its row and be refused.
//
// Kept for that window. An ADMITTED state-changing call from an already-declared
// linkage refreshes the row, at most once per declarationRefreshEvery
// (min(TTL/4, 1 h)), so a working conversation's row is never old enough for
// such a pass to reclaim it. The cost is one UPDATE per linkage per interval.
// Unlike a pin's or a logical agent's, a declaration's updated_at is no other
// evidence (LogicalAgentIDsFor windows by the pin and logical-agent rows,
// never by it), so refreshing it skews nothing — the reason #525 gave for not
// refreshing every live row does not apply.
//
// UPDATE only, never insert: admission is not declaration. A call admitted
// under the one-conversation exemption must not become durable evidence that
// its linkage declared itself.
func (s *connSession) refreshDeclaration(ctx context.Context, toolName string) {
	id := mcp.LogicalAgentFromCtx(ctx)
	if id == "" || s.sessionState == nil || !slices.Contains(tools.StateChangingToolNames(), toolName) {
		return
	}
	v := s.view()
	if !v.session.PersistState || v.proxySessionID == "" {
		return
	}
	linkage := linkageIDOf(id)
	now := time.Now()
	prev, due := s.logicalAgents.refreshDue(linkage, now, declarationRefreshEvery(v.session.PersistStateTTLMinutes))
	if !due {
		return
	}
	if err := s.sessionState.TouchDeclaredLinkage(v.proxySessionID, linkage); err != nil {
		s.logicalAgents.releaseRefresh(linkage, now, prev)
		s.log().Debug("daemon: refreshing the declared linkage failed", "linkage", logicalAgentLabel(linkage), "err", err)
	}
}

// declarationRefreshEvery is how often an active conversation refreshes its
// durable declaration: a quarter of the TTL, capped at an hour, so a row is
// refreshed several times inside any TTL without a write per call.
func declarationRefreshEvery(ttlMinutes int) time.Duration {
	every := time.Hour
	if q := time.Duration(ttlMinutes) * time.Minute / 4; ttlMinutes > 0 && q < every {
		every = q
	}
	return every
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
// ages out with the TTL like every other expendable row of a serve that is not
// connected when the reaper passes; logical_agent is
// deliberately NOT a source — it records every OBSERVED identity, so an
// invented id that made one admitted read would come back declared.
//
// Retried on a converged degraded recovery (retryRestoreIdentity): the record
// and any declaration written while the connection sat degraded are only in
// hand then.
//
// KNOWN LIMITS, both of which cost one refusal whose remedy (session_start)
// re-declares:
//   - With [session] persist_state off nothing is saved, so nothing comes back.
//   - declared_linkage ages out with persist_state_ttl_minutes, but only when an
//     idle-reaper pass finds the serve disconnected (connected sessions are
//     exempt, and nothing is pruned at daemon start, #525). refreshDeclaration
//     keeps a WORKING conversation's row young (see its slack); one idle past
//     the TTL at such a pass, that is not the identity record's linkage, comes
//     back undeclared.
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
