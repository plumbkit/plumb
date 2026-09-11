package tools

// session_start_identity.go — who is calling session_start, resolved before
// anything else the call does.
//
// session_start carries the only identity channels a multiplexing client has.
// `session_id` links the plumb session to the caller's own conversation (making
// it addressable by name from plumb mail and the peer wake hook) AND, on a
// connection shared by several logical agents, names which agent this call
// belongs to — the fact the daemon needs before it decides whose workspace pin a
// re-pin may move (issue #182). Both live here so the two readings of the one
// argument can never drift apart.

import (
	"context"
	"encoding/json"
)

// WithExternalID wires the external-ID linker: fn receives the session_id
// argument, persists it on the session file, and may return an inherited
// session name (non-empty when a matching ended session was found). Nil-safe.
// Returns the receiver for chaining.
func (t *SessionStart) WithExternalID(fn func(id string) string) *SessionStart {
	t.externalIDFn = fn
	return t
}

// WithDeclaredAgent wires the logical-agent identity channel that session_start
// itself carries: fn receives the `session_id` argument and returns the ctx the
// rest of THIS call — the workspace re-pin above all — must run under, so the
// daemon can attribute it to the agent that just declared itself.
//
// It exists because a multiplexing client's subagent declares its identity and
// names its workspace in ONE call, and cannot inject a per-call `_meta` identity
// (Claude Code's `_meta` carries a tool-use id and a progress token, nothing
// agent-scoped). Without this channel the re-pin runs unattributed, lands on the
// CONNECTION rather than the calling agent's shard, and drags every peer agent —
// including the coordinator that pinned first — to the subagent's workspace.
// That is issue #182 as it is actually met in the field. Nil-safe; the ctx is
// returned unchanged when no session_id was passed, so a single-agent connection
// is untouched. Returns the receiver for chaining.
func (t *SessionStart) WithDeclaredAgent(fn func(ctx context.Context, id string) context.Context) *SessionStart {
	t.declaredAgent = fn
	return t
}

// resolveLinkage reports whether the caller passed a non-empty session_id
// (linked) — the external id that makes this session addressable by name from
// plumb mail and the peer wake hook — and, when so, the name inherited from a
// previous session with the same external id (see WithExternalID). linked is
// derived from the raw input regardless of whether an externalIDFn is wired;
// the accessor is consulted only when it is non-nil.
func (t *SessionStart) resolveLinkage(raw json.RawMessage) (inheritedName string, linked bool) {
	id := SessionIDArg(raw)
	if id == "" {
		return "", false
	}
	if t.externalIDFn != nil {
		inheritedName = t.externalIDFn(id)
	}
	return inheritedName, true
}

// LinkageState is what the CONNECTION actually knows about its own external
// linkage and recovery, as opposed to what this particular session_start call
// declared. The distinction is the false-warning fix: a bare session_start on
// a session that linked its conversation an hour ago must not be told it has
// no external id, and a degraded connection must say so on every orientation
// even though nothing about the call in flight is unusual.
type LinkageState struct {
	// ExternalID is the linkage persisted on the session — from this call or
	// any earlier one. Empty means the session is genuinely unlinked (or the
	// accessor is unwired).
	ExternalID string
	// Recovery is this connection's identity-recovery outcome — the same value
	// the initialize _meta carries: established, restored, degraded, or
	// unavailable. Empty before the handshake or without a proxy credential.
	Recovery string
}

// WithLinkageState wires the accessor for the connection's persisted linkage
// and recovery outcome, so the linkage notes key on ACTUAL state rather than
// on whether this particular call carried a session_id. Nil-safe: unwired ⇒
// the notes fall back to keying on this call's arguments alone. Returns the
// receiver for chaining.
func (t *SessionStart) WithLinkageState(fn func() LinkageState) *SessionStart {
	t.linkageStateFn = fn
	return t
}

// linkage returns the connection's actual linkage state, zero when unwired.
func (t *SessionStart) linkage() LinkageState {
	if t.linkageStateFn == nil {
		return LinkageState{}
	}
	return t.linkageStateFn()
}

// WithResumedNewIdentity wires the flag that says this call resumed a
// predecessor's NAME via its external id while running under a NEW internal
// session ID. The resume-by-linkage path never adopts the predecessor's ID —
// only the proxy credential can do that — so the caller is a continuation that
// cannot fully prove itself, and the identity line must say what did not
// follow it: mail and threads bound to the predecessor ID. Nil-safe. Returns
// the receiver for chaining.
func (t *SessionStart) WithResumedNewIdentity(fn func() bool) *SessionStart {
	t.resumedNewIDFn = fn
	return t
}

// resumedNewIdentity reports the name-only-resume flag, false when unwired.
func (t *SessionStart) resumedNewIdentity() bool {
	return t.resumedNewIDFn != nil && t.resumedNewIDFn()
}

// unlinkedSessionNotice is the exact identity-block line session_start emits
// when the caller is genuinely unlinked — its persisted external id is empty.
// Pinning the full string keeps the wording — and therefore the promise it
// makes — stable. The promise changed with the linkage-state work (C5): the
// line states the future cost (a client restart forks the identity and
// strands mail) and, since PLAN-353, also what still WORKS — the session has
// a name and is addressable by it — so an unlinked agent is not told its
// collaboration is unavailable when only continuity is.
const unlinkedSessionNotice = "NOTE: this session has no external id. It has a name and peers can leave_note to it now, " +
	"but a client restart will start a NEW identity (new session ID), and mail or threads addressed to " +
	"this one will not follow you. Pass a stable session_id to session_start to link this conversation " +
	"(on Claude Code, `plumb hooks install claude-code` fills it on every call).\n"

// linkageNote renders what the caller must know about its own linkage and
// recovery state, keyed on the CONNECTION's persisted state rather than on
// what this particular call declared. Two sentences, mutually exclusive,
// rendered in both packet flavours:
//
//   - Degraded first: the connection's proven identity could not be applied
//     this time. The reconnect note says it once, on the reconnect; this says
//     it on every session_start, because a degraded connection outlives that
//     one-shot note — the whole point of C3's visibility work.
//   - Unlinked: the session's persisted external id is empty. The one state
//     with a future cost, so it warns — and the wording states the cost, not
//     just today's addressability gap.
//
// With the accessor unwired, falls back to keying on `linked` alone — the
// only fact available — so a test that does not care still gets the legacy
// behaviour. Rendered from session_start_sections.go and the brief packet.
func (t *SessionStart) linkageNote(linked bool) string {
	if t.linkageStateFn == nil {
		if !linked {
			return unlinkedSessionNotice
		}
		return ""
	}
	st := t.linkage()
	if st.Recovery == "degraded" {
		return "NOTE: identity recovery could not fully apply your proven identity this time — you are running " +
			"under a temporary one. It is retried automatically (three bounded attempts); if they exhaust, a later " +
			"reconnect restores it. Mail addressed to your previous name may not reach you until then.\n"
	}
	if st.ExternalID == "" {
		return unlinkedSessionNotice
	}
	return ""
}

// SessionIDArg extracts the `session_id` argument, or "" when absent or the
// input does not parse. This is THE reader of the argument: resolveLinkage and
// withDeclaredAgent use it for Execute's own identity resolution, and cli's
// onBeforeTool uses it to decide which calls the pre-Execute pin must defer
// for (PLAN-395) — deferring exactly the calls Execute will attribute is only
// correct if both sides read the argument identically, so there is one reader.
func SessionIDArg(raw json.RawMessage) string {
	var a struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return ""
	}
	return a.SessionID
}

// withDeclaredAgent derives the ctx the rest of this call runs under from the
// caller's declared `session_id` (see WithDeclaredAgent). Unchanged when no
// channel is wired or no session_id was passed.
func (t *SessionStart) withDeclaredAgent(ctx context.Context, raw json.RawMessage) context.Context {
	if t.declaredAgent == nil {
		return ctx
	}
	id := SessionIDArg(raw)
	if id == "" {
		return ctx
	}
	return t.declaredAgent(ctx, id)
}
