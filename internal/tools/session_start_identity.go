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
	id := sessionIDArg(raw)
	if id == "" {
		return "", false
	}
	if t.externalIDFn != nil {
		inheritedName = t.externalIDFn(id)
	}
	return inheritedName, true
}

// sessionIDArg extracts the `session_id` argument, or "" when absent or the
// input does not parse. Shared by resolveLinkage and withDeclaredAgent so the
// identity the session record is linked to and the identity the re-pin is
// attributed to can never be read from the argument differently.
func sessionIDArg(raw json.RawMessage) string {
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
	id := sessionIDArg(raw)
	if id == "" {
		return ctx
	}
	return t.declaredAgent(ctx, id)
}
