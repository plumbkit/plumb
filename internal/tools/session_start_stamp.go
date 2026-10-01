package tools

import "context"

// session_start_stamp.go — whether this caller's PER-CALL identity channel is
// actually live, reported at orientation instead of at the first refused write.
//
// The shared-connection write gate reads ONE channel: the per-call
// logical-agent identity (internal/cli/conn_logical_agent.go, refuse). A
// session_start `session_id` registers the agent — which is what makes the
// connection count as shared — but deliberately does not admit a later
// anonymous call (PLAN-394 removed that fallback, because admitting a call on
// the strength of an identity it did not present is attribution by guesswork).
//
// The two facts therefore come apart, and a client can sit in the gap: it
// declares its identity through session_start perfectly well, and every
// state-changing call it makes is still refused. Observed on
// `local-agent-mode-plumb` (Claude desktop's connector). Its runtime DOES apply
// the PreToolUse `updatedInput` rewrite — a hook-added session_id arrives — but
// forwards only the argument keys a tool's schema declares, so the undeclared
// reverse-DNS stamp was dropped. The daemon now advertises a declarable stamp
// key to that client (mcp.ArgLogicalAgentDeclaredKey); this note remains the
// disclosure for any client that still arrives unstamped.
//
// That gap used to be discoverable only by being refused mid-session, behind a
// remedy line naming a hook the user had already installed and which could not
// help. session_start is itself stamped when the channel works
// (claudePreToolUseOutput stamps every `mcp__plumb__*` call, session_start
// included), so THIS call is the honest probe: if it arrived unstamped, the
// channel is not live for this client. The probe must read the identity the
// call carried PER CALL — Execute captures it before the declared session_id
// is applied to the ctx, or the hook's own session_id masks the absence. That is an observation, not a guess, and
// it costs nothing on a client whose channel works. PLAN-440 acceptance (b).

// StampChannelState is what the connection knows about the per-call identity
// channel at the moment session_start runs.
type StampChannelState struct {
	// Shared reports that this connection already serves more than one logical
	// agent, so the write gate is armed and an unattributable state-changing
	// call is being refused now, not hypothetically.
	Shared bool
	// PerCallStamped reports that THIS session_start call carried a per-call
	// logical-agent identity — the channel the write gate reads.
	PerCallStamped bool
	// HookClient reports a client the Claude Code identity hook stamps. The
	// dormant notice (nothing refused yet) is shown only to such a client,
	// whose unstamped call means a missing or broken hook; for any other client
	// on a single-agent connection it would be noise on every session_start.
	HookClient bool
}

// WithStampChannel wires the accessor for the per-call identity channel's
// observed state. Nil-safe: unwired ⇒ silence, so a caller that does not supply
// it (every existing test, and any client whose transport cannot be inspected)
// is unaffected. Returns the receiver for chaining.
func (t *SessionStart) WithStampChannel(fn func(ctx context.Context) StampChannelState) *SessionStart {
	t.stampChannelFn = fn
	return t
}

// stampChannelRefusedNotice is emitted when the gate is already armed: the
// connection is shared and this call carried no per-call identity, so writes
// are being refused right now.
//
// It names the hook first: since the desktop connector's schemas declare
// plumb_agent, the hook works there too, so it is the remedy most callers can
// apply. The transport remedy (one plumb serve per agent) follows, for a client
// that cannot stamp at all.
//
// For a hook that is already installed it names bare `plumb hooks`, which
// reports a missing or stale hook and a daemon that cannot take the stamp. It
// says "checks", not "says why": with a current hook and a daemon that accepts
// the stamp it has nothing to report, so it cannot promise a diagnosis.
const stampChannelRefusedNotice = "NOTE: state-changing calls from this session are being refused. " +
	"This connection serves more than one logical agent and this call carried no per-call identity, " +
	"so plumb cannot tell which agent's workspace a write belongs to and will not guess. Your session_id " +
	"declaration IS recorded, but it identifies this call only. Stamp every call: on Claude Code, " +
	"`plumb hooks install claude-code` (on Claude desktop, restart the app after upgrading plumb); if the hook is " +
	"installed, `plumb hooks` checks it and the daemon. A client whose transport can set " +
	"it sends a per-call _meta identity; otherwise run one plumb serve per agent.\n"

// stampChannelDormantNotice is emitted when this call carried no per-call
// identity but the connection is still single-agent. Nothing is refused yet,
// so the wording states the future cost rather than a present failure.
const stampChannelDormantNotice = "NOTE: this call carried no per-call logical-agent identity. Nothing is " +
	"refused while you are the only agent on this connection, but once a second agent attaches, your " +
	"unstamped state-changing calls are refused. On Claude Code, `plumb hooks install claude-code` stamps " +
	"every call; if the hook is installed, `plumb hooks` checks it and the daemon.\n"

// stampChannelNote renders the disclosure, or "" when there is nothing to say:
// the accessor is unwired, or the channel is live. Rendered alongside
// linkageNote in both packet flavours.
func (t *SessionStart) stampChannelNote(ctx context.Context) string {
	if t.stampChannelFn == nil {
		return ""
	}
	st := t.stampChannelFn(ctx)
	if st.PerCallStamped {
		return ""
	}
	if st.Shared {
		return stampChannelRefusedNotice
	}
	if !st.HookClient {
		return ""
	}
	return stampChannelDormantNotice
}

// mailClaimable reports whether THIS caller may take exactly-once delivery of
// the connection's mail.
//
// check_messages is gated on a shared connection because delivery is
// exactly-once: an unattributable poll consumes a message addressed to somebody
// else. session_start's own Messages block performs the identical claim, and it
// is NOT gated — session_start must stay callable or identity becomes
// undeclarable. So the gate on check_messages was doing half a job: it blocked
// the legitimate read while the consuming path stayed wide open through
// orientation, which is worse than either gating both or gating neither.
//
// The claim is therefore skipped for exactly the callers check_messages refuses.
// They still get the rest of the packet, and their mail stays in the mailbox for
// whoever can prove it is theirs, instead of being silently consumed by a caller
// that cannot.
func (t *SessionStart) mailClaimable(ctx context.Context) bool {
	if t.stampChannelFn == nil {
		return true
	}
	st := t.stampChannelFn(ctx)
	return st.PerCallStamped || !st.Shared
}
