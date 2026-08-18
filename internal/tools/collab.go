package tools

import (
	"strings"
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
	// Intents gates share_intent; Mailbox gates leave_note and check_messages.
	Intents bool
	Mailbox bool
	// KnowledgeHandoff gates share_findings.
	KnowledgeHandoff bool
	// IntentTTLMinutes is the shared TTL for intents and notes.
	IntentTTLMinutes int
	// CrossProject allows messages to and from sessions pinned to a DIFFERENT
	// workspace. It is read as the RECIPIENT's gate: a session only reads the
	// daemon-level cross-project store when its own project sets this, so one
	// project can never push text into another's context uninvited.
	CrossProject bool
	// MaxExchanges caps how many notes one conversation may hold before further
	// replies are refused — the backstop against two agents answering each other
	// indefinitely with no human in the loop.
	MaxExchanges int
	// ChatBudgetBytes caps a single delivered message body. Separate from the
	// hint budget: a message is content the agent must act on, not a pointer.
	ChatBudgetBytes int
	// MaxWaitSeconds caps check_messages' blocking wait, keeping it under the
	// client's own MCP call timeout.
	MaxWaitSeconds int
}

// Defaults mirroring the compiled [collab] values, applied when a resolved
// policy carries a non-positive number so a misconfiguration degrades to sane
// behaviour rather than to zero (which would mean "refuse everything" or "never
// wait").
const (
	defaultMaxExchanges   = 10
	defaultChatBudgetByte = 2048
	defaultMaxWaitSeconds = 55
)

// unregisteredSessionRefusal refuses a broadcast from a session that never
// registered, returning a message for the agent, or "" when it did.
//
// IT MUST TEST THE SESSION ID, NOT THE NAME. An unregistered session still has
// a name: newConnSession logs "continuing unregistered and unaddressable" and
// then assigns `reg.Name = session.GenerateName()` anyway, so the display name
// is populated for the TUI and the logs while the ID is empty. The first
// version of this guard read the name and therefore never fired — it shipped
// inert. Every other gate in the codebase already keys on the ID
// (addressableName, check_messages, workspace_sessions); this one now matches.
//
// What the guard is for: a session with no ID writes rows nothing can attribute
// or clean up, and they OUTLIVE the call. share_intent stores AuthorID = "",
// and ClearSessionIntents returns early on an empty id, so the claim is never
// cleared at session end and lingers for the whole TTL — worse, PutIntent's
// replace step is `DELETE ... WHERE author_id = ?`, so one unregistered
// session's intent deletes every other unregistered session's. share_findings
// writes a project memory whose provenance nobody can trace or ask about.
//
// Refusing costs a caller that is simply early nothing: session_start registers
// the session, and the refusal says so.
func unregisteredSessionRefusal(tool, sessionID string) string {
	if strings.TrimSpace(sessionID) != "" {
		return ""
	}
	return tool + " needs a registered session: this connection has no session identity yet, so " +
		"what it writes could not be attributed to it, replaced by it, or cleaned up when it " +
		"ends — and those records outlive this call. Call session_start first."
}

func (p CollabPolicy) maxExchanges() int {
	if p.MaxExchanges > 0 {
		return p.MaxExchanges
	}
	return defaultMaxExchanges
}

// ChatBudget is the per-message byte cap, falling back to the compiled default
// when unset. Exported because the connection-side delivery path renders
// messages too.
func (p CollabPolicy) ChatBudget() int {
	if p.ChatBudgetBytes > 0 {
		return p.ChatBudgetBytes
	}
	return defaultChatBudgetByte
}

func (p CollabPolicy) maxWaitSeconds() int {
	if p.MaxWaitSeconds > 0 {
		return p.MaxWaitSeconds
	}
	return defaultMaxWaitSeconds
}

// CollabDeps bundles the dependencies for share_intent and leave_note so the
// constructors stay small and the wiring is uniform.
type CollabDeps struct {
	// Workspace returns the connection's pinned workspace root ("" pre-attach).
	Workspace func() string
	// SessionName returns this session's display name (the author label).
	SessionName func() string
	// SessionID is this session's stable ID (intent replace + session-end clear).
	// An accessor, so an ID adopted during initialize (PLAN-296) is seen live.
	SessionID func() string
	// Policy returns the resolved [collab] snapshot.
	Policy func() CollabPolicy
	// Store opens (creating on first use) the workspace's collab.db and returns
	// the handle, or nil when no workspace is attached or the store cannot open.
	// The collab write tools are the ONLY paths that create collab.db, so a
	// workspace whose intents+mailbox flags stay off never gets one.
	Store func() *collab.Store
	// StoreIfExists returns the workspace's collab.db ONLY when it already exists,
	// never creating it — the accessor every read and delivery path must use so a
	// workspace that has not used the feature stays clean. May be nil.
	StoreIfExists func() *collab.Store
	// GlobalStore opens (creating on first use) the daemon-level cross-project
	// store. Only the send path calls it, and only once a message is known to
	// cross a project boundary, so a daemon whose sessions never talk across
	// projects never materialises the file. May be nil.
	GlobalStore func() *collab.Store
	// GlobalStoreIfExists returns the daemon-level store ONLY when it already
	// exists, so delivery never creates it. May be nil.
	GlobalStoreIfExists func() *collab.Store
	// Notifier is the daemon-wide wake-up signal: bumped on send, watched by the
	// piggyback fast path and by check_messages' blocking wait. May be nil.
	Notifier *collab.Notifier
	// ResolvePeer reports the LIVE session answering to a name. May be nil, in
	// which case every message is treated as same-project and bound by name only.
	ResolvePeer func(name string) (PeerSession, bool)
	// InheritedSessionIDs returns predecessor session IDs this session provably
	// continues, so it can still read mail bound to the session a daemon restart
	// ended. May be nil.
	InheritedSessionIDs func() []string
	// TargetAllowsCrossProject reports whether the project pinned at workspace
	// has opted in to [collab] cross_project — the RECIPIENT's consent, resolved
	// from just its path, no live connection to that project required. nil is
	// treated as "cannot confirm consent" and refuses, never as "allowed": the
	// alternative is a message that is accepted and reported sent while the
	// recipient project silently never reads the daemon-level store it lives in,
	// so the sender is told success for something that will sit unclaimed until
	// it expires. May be nil only where the cross-project send path is never
	// exercised (e.g. a test that stays same-project).
	TargetAllowsCrossProject func(workspace string) bool
}

// sessionID returns the session ID, or "" when unwired (tests / pre-registration).
func (d CollabDeps) sessionID() string {
	if d.SessionID == nil {
		return ""
	}
	return d.SessionID()
}

// PeerSession is a live peer session resolved by name.
//
// Concurrency: a value type — safe to copy and read from any goroutine.
type PeerSession struct {
	// Workspace is the root the peer is pinned to. It decides whether a message
	// is same-project (the workspace store) or cross-project (the daemon-level
	// one).
	Workspace string
	// ID is the peer's stable session ID, which a message is bound to so that
	// only that session can read it. A resolver reports found=false — rather
	// than guessing — when no live session answers to the name or when more than
	// one does, and the message then falls back to name-only addressing.
	ID string
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
