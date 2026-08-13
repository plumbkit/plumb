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
	SessionID string
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
	// PeerWorkspace reports the workspace a named peer session is pinned to, and
	// whether such a session is currently known. It decides whether a message is
	// same-project (the workspace store) or cross-project (the daemon-level one).
	// May be nil, in which case every message is treated as same-project.
	PeerWorkspace func(name string) (workspace string, found bool)
	// ClientName returns the MCP client's reported name (""/nil when unknown).
	// Wired for the same reason session_start and daemon_info take it: the
	// mailbox tools name each other, and whether the OTHER half is reachable
	// depends on the client — see nameLeanToolsOnly.
	ClientName func() string
	// ToolProfile returns this connection's resolved tool profile ("lean" or
	// "full"). May be nil, treated as "full" — the permissive answer, matching
	// SessionStart.resolvedToolProfile's unwired default.
	ToolProfile func() string
}

// nameLeanToolsOnly reports whether text these tools emit — a tool description
// or a result — must confine itself to the lean tool set. The rule lives in
// leanNamingOnly; this supplies the collab half of its inputs.
//
// It matters here because the mailbox pair is NOT lean: leave_note's result and
// description both point at check_messages, and under either reason
// leanNamingOnly covers, that pointer is broken. Nil-safe, so the pure-metadata
// constructions in the tools/list budget test (NewLeaveNote(CollabDeps{})) get
// the full text rather than a panic.
func (d CollabDeps) nameLeanToolsOnly() bool {
	return leanNamingOnly(d.ToolProfile != nil && d.ToolProfile() == "lean", d.ClientName)
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
