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
// `local-agent-mode-plumb`, whose runtime does not apply Claude Code's
// PreToolUse `updatedInput` rewrite to MCP calls, so the hook's stamp — emitted
// correctly by `plumb hooks run-claude` — never reaches the daemon.
//
// That gap used to be discoverable only by being refused mid-session, behind a
// remedy line naming a hook the user had already installed and which could not
// help. session_start is itself stamped when the channel works
// (claudePreToolUseOutput stamps every `mcp__plumb__*` call, session_start
// included), so THIS call is the honest probe: if it arrived unstamped, the
// channel is not live for this client. That is an observation, not a guess, and
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
// It deliberately does NOT repeat the refusal's own remedy. That line leads
// with `plumb hooks install claude-code`, which is correct for Claude Code's
// terminal client and actively misleading here — the hook can be installed,
// matched, and emitting the right document while the client drops it. Naming a
// remedy the user has already applied is what turns a five-minute diagnosis
// into an afternoon. The transport remedy is the one that does not depend on
// the client honouring anything.
const stampChannelRefusedNotice = "NOTE: state-changing calls from this session are being refused. " +
	"This connection serves more than one logical agent and this call carried no per-call identity, " +
	"so a write cannot be attributed to the agent that issued it. Your session_id declaration IS " +
	"recorded — it is not the channel the write gate reads. If your client does not carry a per-call " +
	"identity (its runtime may drop a PreToolUse argument rewrite), no hook can close this and the " +
	"refusal has no remedy you can apply: run one plumb serve per logical agent, set " +
	"`[collab] allow_unidentified_writes = true` in your GLOBAL config to accept the attribution risk " +
	"on this machine, or make writes through your client's own file tools.\n"

// stampChannelDormantNotice is emitted when the channel is not live but the
// connection is still single-agent. Nothing is refused yet, so the wording
// states a future cost rather than a present failure — the distinction the
// linkage notes already draw, and the reason acceptance (b) asks for
// orientation-time disclosure rather than a louder refusal.
const stampChannelDormantNotice = "NOTE: this call carried no per-call logical-agent identity. Nothing is " +
	"refused while you are the only agent on this connection, but as soon as a second agent attaches, " +
	"every state-changing call arriving without one will be refused. If your client cannot stamp calls, " +
	"run one plumb serve per logical agent.\n"

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
