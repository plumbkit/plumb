package tools

// session_start_self.go — telling the caller who IT is.
//
// Orientation named everyone except the reader. The identity block printed a
// "Session:" line only when a name was INHERITED from a resumed conversation,
// so an ordinary first contact got nothing — while the peer digest right below
// it confidently listed the other agents by name. An agent that reads its own
// orientation and finds exactly one name in it will use that name for itself,
// and the one name on offer belongs to a peer. That is not a hypothetical: it
// is what happened, and the resulting "my name changed after the reconnect"
// report is what this file exists to prevent.
//
// The rule is therefore unconditional. Both packets state the caller's own
// current name and internal session ID, explicitly labelled as its own, on
// first contact and on every later call — and both render it through the SAME
// function, because the previous arrangement had two independent renderings of
// the identity block and they had already drifted.

import (
	"fmt"
	"strings"
)

// WithSelfIdentity wires the accessor for this connection's own current name.
// Paired with WithSelfSession (the ID), it is what lets orientation say who the
// caller is. Nil-safe: unwired ⇒ no self line, which is how a test that does not
// care keeps its expected output. Returns the receiver for chaining.
func (t *SessionStart) WithSelfIdentity(name func() string) *SessionStart {
	t.selfName = name
	return t
}

// selfIdentityLine renders the caller's own identity, or "" when this connection
// has none to report.
//
// resumedName is the name inherited from a matching ended session, when
// session_start's external-ID linkage found one; it is preserved as a suffix
// because a resumed conversation genuinely is a different fact from a fresh one,
// and PR #189's bootstrap guarantee turns on the caller seeing it.
//
// The ID is included and abbreviated. Included, because the name is the mailbox
// address and the ID is what a message is BOUND to, and an agent debugging why a
// note did not arrive needs both. Abbreviated, because the full value adds ~24
// bytes to a packet with a hard budget and its only use here is recognition —
// the exact ID is available from daemon_info, which the note points at.
//
// "you" is spelled out rather than implied. "Session: azure-falcon" beside a
// peer list containing azure-falcon is exactly the ambiguity that produced the
// incident; a label that cannot be misread costs four characters.
func (t *SessionStart) selfIdentityLine(resumedName string) string {
	name, id := t.selfIdentity()
	if name == "" {
		// No self accessor wired, or an unnamed session. Fall back to the line
		// that shipped before this one existed, so a caller that resumed a
		// conversation is still told the name it resumed under — that is PR
		// #189's bootstrap guarantee, and it must not be collateral damage of
		// adding a stronger line beside it. With neither, there is nothing
		// truthful to say.
		if resumedName == "" {
			return ""
		}
		return "Session:  " + resumedName + " (resumed)\n"
	}
	var sb strings.Builder
	sb.WriteString("Session:  " + name + " (you")
	if id != "" {
		sb.WriteString(", id " + shortSessionID(id))
	}
	sb.WriteString(")")
	if resumedName != "" {
		sb.WriteString(" — resumed")
		if resumedName != name {
			// The inherited name did not stick (a live peer holds it). Saying so
			// is the point: silently showing the name it actually has, with no
			// hint that the requested one was refused, is how a caller concludes
			// the session_id argument did nothing.
			fmt.Fprintf(&sb, "; requested %s, which is in use", resumedName)
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// selfIdentity returns the caller's own name and session ID, either possibly
// empty when the accessor is unwired or the session is unregistered.
//
// An unregistered session (empty ID) still has a display name, but it was drawn
// without a uniqueness check and no peer's check can see it — so it is not an
// address. Reporting the name with no ID beside it is honest about exactly that:
// the caller sees what it is called without being invited to treat it as
// routable.
func (t *SessionStart) selfIdentity() (name, id string) {
	if t.selfName != nil {
		name = t.selfName()
	}
	if t.selfSessID != nil {
		id = t.selfSessID()
	}
	return name, id
}

// shortSessionIDLen is how much of a session ID the identity line shows. Session
// IDs are hex; eight characters is the familiar short-hash length and is ample
// to recognise one value among the handful a workspace has live.
const shortSessionIDLen = 8

// shortSessionID abbreviates a session ID for display, returning it unchanged
// when it is already short enough.
func shortSessionID(id string) string {
	if len(id) <= shortSessionIDLen {
		return id
	}
	return id[:shortSessionIDLen] + "…"
}
