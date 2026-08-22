package cli

// conn_logical_agent.go — observation of the logical-agent identities a
// connection declares, the shared-connection signature that keys on them, and
// the fail-closed ceiling that refuses what cannot be attributed.
//
// PLAN-286 (§3) promotes a stable, client-supplied logical-agent ID into the
// primary identity key. One `plumb serve` connection may multiplex several
// logical agents, and the daemon can only tell them apart — or refuse to share
// state — by the IDs they declare. The ID arrives on two channels:
// session_start's `session_id` argument (stable across reconnects, recorded at
// attach) and a per-call `tools/call._meta[MetaLogicalAgentKey]` (recorded per
// call, for clients that cannot set session_id at attach time).

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tools"
)

// logicalAgentState tracks the distinct logical-agent IDs observed on one
// connection. Guarded by mu because a multiplexing client issues tool calls
// concurrently; observing a new ID must be safe to interleave with any other
// read of the set.
type logicalAgentState struct {
	mu sync.Mutex
	// seen is the set of distinct IDs declared so far (attach-time session_id
	// and per-call _meta alike). It only grows: an ID a client has declared stays
	// declared for the connection's life, so a later re-check cannot un-see it
	// and flip the shared flag back off.
	seen map[string]struct{}
	// attachID is the session_id recorded at attach. It is the fallback identity
	// for a call that carries no per-call _meta: on a shared connection, such a
	// call is attributed to the attach-time agent, not refused.
	attachID string
}

func (l *logicalAgentState) record(id string, attach bool) bool {
	if id == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = make(map[string]struct{})
	}
	l.seen[id] = struct{}{}
	if attach {
		l.attachID = id
	}
	return len(l.seen) > 1
}

// isShared reports whether two or more distinct logical-agent IDs have been
// observed on this connection. It is the gate for per-agent keying: below it the
// connection itself is the identity and no shard is ever created.
func (l *logicalAgentState) isShared() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen) > 1
}

// attachIdentity returns the attach-time session_id fallback identity (last one
// recorded), or "". It is the attribution for a call carrying no per-call _meta.
func (l *logicalAgentState) attachIdentity() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.attachID
}

// refuse reports whether a call declaring callID must be refused on this
// connection: the connection is shared (two or more distinct IDs observed) and
// the call is unattributable (no per-call ID, and no attach-time session_id to
// fall back on). A non-shared connection needs no ID — the connection itself is
// the identity.
func (l *logicalAgentState) refuse(callID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) <= 1 {
		return false
	}
	if callID != "" {
		return false
	}
	return l.attachID == ""
}

// recordLogicalAgentAttach records a session_start.session_id identity.
func (s *connSession) recordLogicalAgentAttach(id string) { s.recordLogicalAgent(id, true) }

// recordLogicalAgentCall records a per-call tools/call._meta identity.
func (s *connSession) recordLogicalAgentCall(id string) { s.recordLogicalAgent(id, false) }

// declaredAgentCtx is the third identity channel: the `session_id` a caller
// declares INSIDE session_start, promoted to this call's logical-agent identity.
//
// The two channels above arrive before the tool runs — _meta is parsed at
// dispatch, and an attach-time session_id from a PREVIOUS call is already
// recorded. Neither covers the case the field actually produces: a subagent
// whose first contact declares its identity and names its workspace in one call,
// over a client that cannot inject a per-call _meta. Without an identity on that
// call's ctx, repinShard declines it and the re-pin runs on the CONNECTION,
// moving every peer agent's workspace with it (issue #182). Attributing it to
// the id the caller just declared keeps the move on that agent's own shard.
//
// A per-call _meta identity, being the stronger channel (it is asserted per
// call rather than per attach), always wins.
func (s *connSession) declaredAgentCtx(ctx context.Context, id string) context.Context {
	if id == "" || mcp.LogicalAgentFromCtx(ctx) != "" {
		return ctx
	}
	return mcp.WithLogicalAgent(ctx, id)
}

// recordLogicalAgent is the single choke point every identity channel feeds
// (session_id at attach, _meta per call), so the shared-connection detection
// sees one consistent view regardless of how the ID arrived. The first time the
// connection becomes shared it is marked for the operator.
func (s *connSession) recordLogicalAgent(id string, attach bool) {
	if s.logicalAgents.record(id, attach) {
		s.markSharedConnectionDetected()
	}
}

// markSharedConnectionDetected records the shared-connection condition on the
// session record so it is visible to an operator, not merely refused per call.
// It is deliberately NOT "blocked": the hard refusal is reserved for the
// anonymous call path below. Per-agent keying (step 2) is in effect, so
// distinct-ID agents no longer share pin/trackers; anonymous state-changing
// calls are still refused because they cannot be attributed.
func (s *connSession) markSharedConnectionDetected() {
	s.log().Warn("daemon: shared connection detected — multiple logical agents multiplexed over one serve; per-agent state is isolated, anonymous state-changing calls are refused")
	if s.sessionID() == "" {
		return
	}
	session.Patch(s.sessionID(), func(info *session.Info) {
		info.Health = "shared_connection_detected"
		info.HealthMessage = "multiple logical agents share this connection; per-agent state is isolated, anonymous state-changing calls are refused — run one plumb serve per logical agent"
	})
}

// refuseSharedStateChange is the fail-closed ceiling. It refuses a mutating
// tool call that arrives on a shared connection without a trustworthy
// logical-agent identity, naming the supported topology and its remedy. Read
// calls are never refused: sharing read-only state is safe, and the acceptance
// contract is about state-changing operations resetting a peer's pin, trackers,
// rate budget, undo state or language.
func (s *connSession) refuseSharedStateChange(_ context.Context, name, logicalAgent string) error {
	if !slices.Contains(tools.WriteToolNames(), name) {
		return nil
	}
	if !s.logicalAgents.refuse(logicalAgent) {
		return nil
	}
	return fmt.Errorf("shared connection: %s is a state-changing call with no logical-agent identity, so it cannot be attributed to one of the agents multiplexing this connection; the supported topology is one plumb serve per logical agent — pass session_start.session_id or a per-call _meta[%s] to identify each agent", name, mcp.MetaLogicalAgentKey)
}
