package tools

import (
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/redact"
)

// collab.go holds the shared plumbing for the phase-2 cross-agent sharing write
// tools (share_intent, leave_note): the resolved [collab] policy snapshot, the
// dependency bundle both tools take, and the small helpers they share (body
// redaction, TTL resolution). Both tools are advisory — they persist an
// agent-authored CLAIM that peers may steer around; nothing they write ever
// blocks a write. Each refuses cleanly when its own [collab] flag is off.

// defaultIntentTTLMinutes mirrors the compiled [collab] intent_ttl_minutes
// default; used when the resolved policy carries a non-positive value so a
// misconfiguration cannot store an instantly-expired row.
const defaultIntentTTLMinutes = 120

// CollabPolicy is the resolved [collab] snapshot the collab write tools consult
// (never a per-call config read — the connection snapshots it on attach/reload).
type CollabPolicy struct {
	// Intents gates share_intent; Mailbox gates leave_note.
	Intents bool
	Mailbox bool
	// KnowledgeHandoff gates share_findings.
	KnowledgeHandoff bool
	// IntentTTLMinutes is the shared TTL for intents and notes.
	IntentTTLMinutes int
}

// CollabDeps bundles the dependencies for share_intent and leave_note so the
// constructors stay small and the wiring is uniform.
type CollabDeps struct {
	// Workspace returns the connection's pinned workspace root ("" pre-attach).
	Workspace func() string
	// SessionName returns this session's display name (the author label).
	SessionName func() string
	// SessionID is this session's stable ID (intent replace + session-end clear).
	SessionID string
	// Policy returns the resolved [collab] snapshot.
	Policy func() CollabPolicy
	// Store opens (creating on first use) the workspace's collab.db and returns
	// the handle, or nil when no workspace is attached or the store cannot open.
	// The collab write tools are the ONLY paths that create collab.db, so a
	// workspace whose intents+mailbox flags stay off never gets one.
	Store func() *collab.Store
}

// resolveTTL turns a minutes count into a duration, applying the policy default
// when overrideMinutes is non-positive and a hard floor when even the policy is
// misconfigured, so a stored row always outlives the call.
func resolveTTL(policyMinutes, overrideMinutes int) time.Duration {
	m := overrideMinutes
	if m <= 0 {
		m = policyMinutes
	}
	if m <= 0 {
		m = defaultIntentTTLMinutes
	}
	return time.Duration(m) * time.Minute
}

// redactBody scrubs likely secrets from an agent-authored body before it is
// persisted, mirroring the episodic-memory rule. Returns the cleaned text and
// whether anything was redacted (so the tool can note it in its reply).
func redactBody(s string) (string, bool) {
	clean, n := redact.Redact(s)
	return clean, n > 0
}
