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
}

// record commits an identity and reports the connection's shared STATE and,
// separately, whether THIS record produced it — the transition from fewer than
// two committed identities to two.
//
// Two returns, not one, because the two consumers need different questions
// answered (PLAN-396). The announcement is an EVENT: the operator needs telling
// once, so re-announcing on every declaration was the noise this card set out to
// remove. The health note is a STATE: it describes a condition that stays true
// for the connection's life, and conn_repin clears Health on ordinary successes
// (the same-root promotion at :279 and the re-pin at :348), so a note that can
// only be written on the transition is gone for good after the first re-pin —
// while the connection is still shared and still refusing anonymous
// state-changing calls with no diagnostic left to explain why. Collapsing both
// onto the transition bool made "announce once" eat "keep the state true".
func (l *logicalAgentState) record(id string) (shared, transition bool) {
	if id == "" {
		return false, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	wasShared := len(l.seen) > 1
	if l.seen == nil {
		l.seen = make(map[string]struct{})
	}
	l.seen[id] = struct{}{}
	shared = len(l.seen) > 1
	return shared, shared && !wasShared
}

// sharedWith reports whether the connection is shared once the caller of THIS
// request is counted — the observed set plus id. It is THE gate for per-agent
// keying: below it the connection itself is the identity and no shard is created.
//
// It counts the caller because a routing decision must not require a commitment.
//
// A subagent's first session_start declares an identity the connection has never
// seen. Recording it up front would make the plain "len(seen) > 1" reading true,
// but `seen` only grows, so a call that is then REFUSED would have flipped the
// connection into per-agent keying for every peer — each peer's next call landing
// on a fresh shard with an empty read tracker, and strict mode rejecting its edits
// with "has not been read". Asking the question hypothetically instead lets the
// refusal leave no trace in the identity set: the commitment happens on the
// success path, through session_start's external-ID linker.
//
// An anonymous call (id == "") has no caller to count, so it asks the plain
// question: are two or more identities already committed.
func (l *logicalAgentState) sharedWith(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) > 1 {
		return true
	}
	if id == "" {
		return false
	}
	if _, ok := l.seen[id]; ok {
		return false
	}
	return len(l.seen) == 1
}

// refuse reports whether a call declaring callID must be refused on this
// connection: the connection is shared (two or more distinct IDs observed) and
// the call is unattributable (no per-call ID). A non-shared connection needs no
// ID — the connection itself is the identity.
//
// PLAN-394 removed the attach-time fallback from this decision. Before it, an
// anonymous call was admitted whenever ANY session_start had attached — and
// shardFor then attributed the call to the agent that attached LAST, so an
// unattributable write landed in a peer's trackers and, after that peer's
// force-pin, in the peer's project. Admitting a call on the strength of an
// identity it did not present is attribution by guesswork; on a shared
// connection only a presented ID admits a state-changing call.
func (l *logicalAgentState) refuse(callID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) <= 1 {
		return false
	}
	return callID == ""
}

// recordLogicalAgentAttach records a session_start.session_id identity.
func (s *connSession) recordLogicalAgentAttach(id string) { s.recordLogicalAgent(id) }

// recordLogicalAgentCall records a per-call tools/call._meta identity.
func (s *connSession) recordLogicalAgentCall(id string) { s.recordLogicalAgent(id) }

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
//
// Nothing is RECORDED here. Putting the id on the ctx is enough for shardFor and
// repinShard to route this call to its own agent (they ask sharedWith, which
// counts the caller hypothetically), and it leaves the identity set untouched if
// the call is then refused. The commitment — the observed set, the attach-time
// fallback identity, the external-ID registration — happens on session_start's
// success path through externalIDFn. An agent whose re-pin was refused never
// attached, and must leave nothing behind: not an identity its peers' anonymous
// calls could inherit, and not a shared-connection flag that resets every peer's
// read tracking.
func (s *connSession) declaredAgentCtx(ctx context.Context, id string) context.Context {
	if id == "" || mcp.LogicalAgentFromCtx(ctx) != "" {
		return ctx
	}
	return mcp.WithLogicalAgent(ctx, id)
}

// recordLogicalAgent is the single choke point every identity channel feeds
// (session_id at attach, _meta per call), so the shared-connection detection
// sees one consistent view regardless of how the ID arrived.
//
// The announcement and the health note are driven by different halves of
// record's answer, because they answer different questions (PLAN-396). The Warn
// fires on the TRANSITION — once per connection, so peers declaring themselves
// do not re-announce a condition the operator has already been told about. The
// health note is re-asserted on every declaration while the connection is
// SHARED, because conn_repin clears Health on ordinary successes and a note
// written only on the transition could never come back — leaving a connection
// that is still shared, and still refusing anonymous state-changing calls, with
// nothing on the session record to explain the refusal.
func (s *connSession) recordLogicalAgent(id string) {
	shared, transition := s.logicalAgents.record(id)
	if !shared {
		return
	}
	if transition {
		s.log().Warn("daemon: shared connection detected — multiple logical agents multiplexed over one serve; per-agent state is isolated, anonymous state-changing calls are refused")
	}
	s.markSharedConnectionDetected()
}

// markSharedConnectionDetected records the shared-connection condition on the
// session record so it is visible to an operator, not merely refused per call.
// It is deliberately NOT "blocked": the hard refusal is reserved for the
// anonymous call path below. Per-agent keying (step 2) is in effect, so
// distinct-ID agents no longer share pin/trackers; anonymous state-changing
// calls are still refused because they cannot be attributed.
//
// It never downgrades a more specific, more actionable note another path has
// written (contested_pin, blocked): Health is a single field per session, and
// before PLAN-396 this mark rewrote it on every identity declaration, making any
// other note's lifetime "until the next peer call". Writing is therefore
// conditional and idempotent — the note lands when Health is empty or already
// this same mark, and re-asserting it is a no-op — which is what lets the caller
// call this on EVERY declaration while shared without the clobbering returning.
// The announcement is separate, and stays on the transition.
func (s *connSession) markSharedConnectionDetected() {
	if s.sessionID() == "" {
		return
	}
	session.Patch(s.sessionID(), func(info *session.Info) {
		if info.Health != "" && info.Health != "shared_connection_detected" {
			return
		}
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
