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
	// AddresseeID binds the note to ONE session: the stable ID of the live
	// session that answered to Addressee when it was sent. Only that session may
	// claim it. Empty means unbound — a pre-v3 row, a note to a peer that had not
	// connected, or an AddresseeNext note — and is delivered by name alone.
	AddresseeID string
	CreatedAt   time.Time
	ExpiresAt   time.Time

	// ConversationID groups a note with its replies into one thread. Minted by
	// the store on a fresh message and echoed back by the recipient to reply.
	// Empty for an intent, and for a legacy row written before schema v2.
	ConversationID string
	// DeliveredAt is the read watermark: zero until the note is claimed by a
	// reader, then the claim time. A delivered note is not re-delivered; it stays
	// in place until its TTL so the conversation transcript (and its exchange
	// count) survives.
	DeliveredAt time.Time
	// DeliveredTo is the display name of the session that claimed the note. For
	// an AddresseeNext note this records who won the race.
	DeliveredTo string
	// DeliveredToID is the stable session ID of the claimant, recorded alongside
	// DeliveredTo so a claim identifies WHO received the note rather than only
	// what they were called. Empty on rows claimed before the column existed,
	// which membershipPredicate treats as "fall back to the name" — the same
	// concession AddresseeID makes.
	DeliveredToID string
	// OriginWorkspace is the sender's workspace root, stamped only on a
	// cross-project note so the recipient can see which project it came from.
	// Empty for a same-project note.
	OriginWorkspace string
	// TargetWorkspace is the workspace the recipient must be pinned to for this
	// note to be claimable. Set on every cross-project note; empty on a
	// same-project one, where the containing collab.db is itself the scope.
	//
	// It exists because a session NAME is not a safe address on its own: names
	// are drawn from a small pool with no uniqueness check, and rename_session
	// lets a session adopt any name it likes. Without this column a session in
	// one project could claim another project's cross-project mail simply by
	// being called the right thing.
	TargetWorkspace string
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

	// AuthorInheritedIDs are predecessor session IDs this author provably
	// continues. They count as the author's own for CONVERSATION MEMBERSHIP, so a
	// session that came back through the authenticated reconnect path can still
	// reply into the threads its predecessor was in. Same reasoning as
	// Claimant.InheritedIDs on the delivery side; optional and empty for a session
	// that inherited nothing.
	AuthorInheritedIDs []string

	// AddresseeID binds the note to the one session that answered to Addressee at
	// send time. Set it ONLY for a peer that is live and resolves to exactly one
	// session; leave it empty otherwise, which keeps the historical name-only
	// addressing that a peer which is not connected depends on. PutNote clears it for
	// AddresseeNext.
	AddresseeID string
	// ConversationID threads this note onto an existing conversation. Empty mints
	// a fresh one, which PutNote returns so the caller can tell the sender what to
	// quote in order to continue the thread.
	ConversationID string
	// OriginWorkspace is the sender's workspace root. Set only when the note
	// crosses a project boundary (it lands in the daemon-level store); left empty
	// for a same-project note in the workspace's own collab.db.
	OriginWorkspace string
	// TargetWorkspace is the workspace the recipient must be pinned to. Required
	// for a cross-project note; empty for a same-project one.
	TargetWorkspace string
	// MaxExchanges caps how many unexpired notes the conversation may hold.
	// PutNote enforces it as part of the insert and refuses an over-budget note
	// with ErrConversationFull; a non-positive value means uncapped. It lives on
	// the input rather than on the Store because the cap comes from the sending
	// connection's [collab] policy, and one Store serves every connection to a
	// workspace.
	MaxExchanges int
}
