// Package collab is the tiny per-workspace store behind plumb's phase-2
// cross-agent sharing signals: agent-declared intents and a minimal mailbox.
//
// Unlike the phase-1 peer-awareness signals (which are verifiable observations
// derived from writes the daemon itself performed or watched), the rows here are
// agent-authored CLAIMS — "I'm refactoring the rate limiter" — so callers always
// render them as claims, distinct from observed facts. Everything is advisory:
// nothing stored here ever blocks a write.
//
// An intent and a note are the same row shape with different targeting:
//
//   - intent — one live intent per session (a new one replaces the old);
//     optionally scoped to path globs describing the area being worked on;
//     broadcast to everyone active on the workspace right now.
//   - note   — a short message addressed to a named peer session, or to "next"
//     (whoever attaches to this workspace next).
//
// Rows carry a TTL and are pruned on the daemon session-reaper tick AND filtered
// from every read regardless of pruning, so a missed prune never resurrects a
// stale row. Intents also die with their author session (cleared on close);
// notes survive their author.
//
// Storage is a small SQLite DB at <workspace>/.plumb/collab.db (WAL,
// auto-gitignored like topology.db), created lazily on first write. A workspace
// where both the intents and mailbox flags stay off never gets a collab.db.
// Losing collab.db loses only expiring advisory data, which is acceptable —
// unlike memory.db it is not a rebuildable index of durable content, so the rows
// deliberately do NOT live there.
package collab

import "time"

// Kind distinguishes the two row shapes stored in collab.db.
type Kind string

const (
	// KindIntent is a broadcast declaration of what a session is working on.
	KindIntent Kind = "intent"
	// KindNote is a message addressed to a named peer session or to "next".
	KindNote Kind = "note"
)

// AddresseeNext is the reserved addressee meaning "whoever attaches to this
// workspace next"; such a note is consumed on first delivery.
const AddresseeNext = "next"

// Row is a stored intent or note. Times are wall-clock; the store persists them
// as Unix-nanosecond integers.
//
// Concurrency: a value type — safe to copy and read from any goroutine.
type Row struct {
	ID            int64
	Kind          Kind
	AuthorSession string   // posting session's display name
	AuthorID      string   // posting session's ID (intent replace + session-end cleanup)
	Body          string   // redacted free text
	PathGlobs     []string // intent only — the area being worked on; nil for a note
	Addressee     string   // note only — a session name or AddresseeNext; "" for an intent
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// IntentInput is the payload for PutIntent. TTL is clamped to a sane minimum by
// the store; PathGlobs may be empty (an unscoped intent).
type IntentInput struct {
	AuthorSession string
	AuthorID      string
	Body          string
	PathGlobs     []string
	TTL           time.Duration
}

// NoteInput is the payload for PutNote. Addressee is a peer session name or
// AddresseeNext (the caller defaults an empty value to AddresseeNext).
type NoteInput struct {
	AuthorSession string
	AuthorID      string
	Body          string
	Addressee     string
	TTL           time.Duration
}
