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
	"strings"
	"sync"
	"time"

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

// seedsConnectionReads decides which agent's new shard inherits the reads the
// connection made before it turned shared: the agent whose id IS the
// connection's linkage — the conversation the session record is linked to,
// which the SessionStart hook and the identity hook both derive from the
// same client id — and only for the root the connection holds.
//
// Not "the first identity this process saw". After a daemon restart the
// connection tracker is rehydrated from the rows persisted under the empty
// agent id (the parent's reads), the proxy replays the pin but no agent id,
// and in the house pattern the parent is parked on the Agent tool while a
// subagent works — so the subagent's stamped call is routinely the FIRST the
// new session sees, and a first-seen rule would hand it the parent's reads,
// letting it edit in strict mode files it never read. The linkage survives
// the restart in the durable identity record, so keying on it does not.
//
// Root too: a shard that restored a pin elsewhere (loadPinForAgent) must not
// resurrect reads for a workspace it is not pinned to; ReadTracker.Reset
// documents that a read is only ever valid for the root it was made under.
func (s *connSession) seedsConnectionReads(id, shardRoot, connRoot string) bool {
	if id == "" || linkageIDOf(id) != id {
		return false
	}
	return id == s.externalID() && shardRoot == connRoot
}

// linkageIDOf returns the conversation half of a logical-agent id. A Claude
// Code subagent is stamped `<conversation>/<agent>` by the PreToolUse hook;
// the session RECORD (external id, name inheritance, `plumb mail
// --external-id`, the Stop-hook wake) is linked to the conversation, so a
// subagent declaring itself never rewrites the linkage its parent owns, while
// the shard machinery keeps the full id. A plain id is its own linkage.
func linkageIDOf(id string) string {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[:i]
	}
	return id
}

// logicalAgentLabel renders a logical-agent id for logs and status lines:
// the first eight characters of the conversation half, plus `/agent-<first
// eight of the agent half>` for a hook-stamped subagent. Nothing is stored;
// the label is derived from the id's shape on every render.
func logicalAgentLabel(id string) string {
	conv, agent := id, ""
	if i := strings.IndexByte(id, '/'); i >= 0 {
		conv, agent = id[:i], id[i+1:]
	}
	label := shortIDPrefix(conv)
	if agent != "" {
		label += "/agent-" + shortIDPrefix(agent)
	}
	return label
}

func shortIDPrefix(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
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
		s.log().Warn("daemon: shared connection detected — multiple logical agents multiplexed over one serve; per-agent state is isolated, anonymous state-changing calls are refused",
			"agent", logicalAgentLabel(id))
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
		info.HealthMessage = "multiple logical agents share this connection; per-agent state is isolated, and a state-changing call carrying no identity is refused — " + sharedIdentityRemedy
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
	return fmt.Errorf("shared connection: %s is a state-changing call with no logical-agent identity, so it cannot be attributed to one of the agents multiplexing this connection — %s", name, sharedIdentityRemedy)
}

// sharedIdentityRemedy names the ways an agent on a shared connection gets an
// identity, cheapest first. Identity comes before topology on purpose
// (PLAN-417): the previous wording led with "one plumb serve per agent", the
// one remedy an agent cannot apply from inside a tool call.
const sharedIdentityRemedy = "each agent must identify itself: on Claude Code, `plumb hooks install claude-code` stamps every call (the PreToolUse identity hook); otherwise pass a stable per-agent session_start.session_id or a per-call _meta[" + mcp.MetaLogicalAgentKey + "]; or run one plumb serve per logical agent"

// linkExternalID is session_start's external-ID linker: it records the
// declared identity, links the session RECORD to its conversation, and
// inherits a predecessor's name when the conversation resumed within the
// grace window. It returns the inherited name, or "".
//
// Two ids are in play and they are deliberately different. The full id
// (`<conversation>` or `<conversation>/<agent>`) is what the shard machinery
// keys on, so a hook-stamped subagent gets its own pin and trackers. The
// LINKAGE is the conversation half only: `plumb mail --external-id`, the
// Stop-hook wake and name inheritance all address the conversation, and a
// subagent declaring itself must not rewrite the linkage its parent owns.
// Before this rooting, any subagent session_id silently replaced the
// connection's external id and the parent's mail stopped resolving.
//
// The inheritance branch runs once per linkage: a session already linked to
// this conversation (the parent already attached; a second subagent attaching)
// has nothing to resume, and re-running the rename against a name the session
// already holds is at best a no-op and at worst a second resumedNewIdentity.
func (s *connSession) linkExternalID(externalID string) string {
	linkage := linkageIDOf(externalID)
	alreadyLinked := linkage != "" && s.externalID() == linkage
	session.SetExternalID(s.sessionID(), linkage)
	s.recordLogicalAgentAttach(externalID)
	// Mirror the linkage into the durable identity record. Until PLAN-426 it
	// lived only in the session JSON, which is collected 24 h after the session
	// ends — so an outage longer than that lost the linkage while the identity
	// itself survived, and `plumb mail --external-id` stopped resolving a
	// session that had in fact recovered.
	s.persistIdentity()
	if alreadyLinked {
		return ""
	}
	prev := session.FindEnded(linkage, 24*time.Hour)
	if prev == nil {
		return ""
	}
	// A predecessor was found: this call resumed its NAME under a NEW internal
	// session ID (the linker never adopts IDs). The flag is what the identity
	// line discloses — see session_start_self.go.
	s.mutate(func(v *sessionView) { v.resumedNewIdentity = true })
	// session.Rename refuses a name a live session already holds, so two resumes
	// racing on one external ID inside the grace window cannot both inherit it —
	// mailbox delivery matches on the name string, and an ambiguous address
	// silently misdelivers. Resuming, not renaming: the entitlement is the
	// external ID the caller just presented, which is what lets a RESTARTED
	// `plumb serve` — new proxy secret, new session ID, same conversation — take
	// back the name its own durable record reserves.
	name, err := s.renameSessionResuming(prev.Name, linkage)
	if err == nil {
		return name
	}
	// Log it: a silently dropped inheritance looks to the caller like the
	// session_id argument did nothing at all.
	s.log().Debug("daemon: could not inherit the previous session name; keeping the generated one",
		"inherited", prev.Name, "err", err)
	return ""
}
