package cli

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/tools"
)

// conn_chat.go — the connection-side PREVIEW of agent-to-agent messages
// ([collab] mailbox): the block appended to a tool result when a peer has
// written to this session.
//
// It shows messages; it does not deliver them. Delivery — setting the read
// watermark — belongs to check_messages and session_start, the two tools the
// model itself calls. This block rides on whatever tool the agent happened to
// call, and no client promises to show the model text it never asked for: a
// harness that runs plumb's tools inside a sandboxed program discards the whole
// result. While this path claimed, that silently consumed the message. See
// internal/tools/chat.go for the full account.
//
// Unlike the memory, peer-activity and intent hints, this runs on EVERY tool
// call rather than only path-bearing ones — a message is about the agent, not
// about the file it happens to be touching, and an agent that only ever calls
// git or run_task must still receive it. That makes the cost of the check the
// design problem, and it is worth being precise about WHERE that cost is,
// because the intuitive answer is wrong.
//
// It is not I/O. A delivery check that finds nothing writes nothing: 200
// consecutive zero-match claims produce 0 bytes of WAL, and the statement itself
// executes in 7.6µs. Nor is taking the writer lock expensive in itself.
//
// The cost is the CLAIM'S QUERY PLAN. `UPDATE … WHERE id IN (SELECT … ORDER BY
// created_at)` compiles to a LIST SUBQUERY plus a temp B-tree, rebuilt on every
// execution to sort a result set that is almost always empty: 100-145µs, all of
// it holding the writer lock. That is roughly twenty times longer than a bare
// write needs the lock, which is the real problem — not the work itself but the
// window it opens. Measured against a peer's leave_note holding that lock for
// 500µs, a colliding claim costs p50 1.42ms, p99 7.07ms and up to 18ms: more
// than an entire read_file, to answer "no".
//
// So there are two guards, cheapest first. The daemon-wide in-process notifier
// answers "has anything happened at all": a send bumps a generation counter for
// the recipient, this path compares its cached counters, and the steady state —
// no mail — is a map lookup under one mutex and nothing else. When a counter has
// moved, or the periodic backstop falls due, a SELECT 1 … LIMIT 1 probe
// (Inbox.HasPending, ~20-30µs, no write lock) answers "is any of it for me",
// because neither trigger is evidence that it is: the backstop fires
// unconditionally, and a session name is a daemon-wide notifier key, so a send
// to a same-named peer in another project bumps this one too. Only a probe hit
// reads the mailbox.
//
// The counters are a fast path, not the truth. They reset when the daemon
// restarts, and a message may have been written by a previous daemon, so the
// periodic full check backstops them: a missed bump costs preview latency, never
// a message — and now, not even delivery, since the message stays claimable
// either way.
//
// What the preview changed about that cost model: a probe hit now runs
// Inbox.Peek (a bounded SELECT, no writer lock) rather than the claim whose
// query plan the paragraphs above are about, so the expensive case is cheaper
// than it was. The new cost is at the other end — an unclaimed note keeps the
// probe answering yes, so a session that never calls check_messages re-runs
// probe-plus-peek once per backstop interval instead of settling at zero after
// one claim. Two indexed reads per 30s per connection, and only while mail is
// genuinely waiting: the empty-mailbox path every call takes is untouched.

// chatFullCheckInterval is how often the database is probed even when the
// notifier reports nothing new. It bounds the worst-case delivery delay after a
// daemon restart (which zeroes the counters) without putting a query on the hot
// path in the common case. What it now triggers is the probe, not the claim, so
// the periodic cost of an empty mailbox is one indexed lookup rather than a
// write-locked statement that updates nothing.
const chatFullCheckInterval = 30 * time.Second

// maxPreviewedTracked bounds the previewed-key set. It is only ever as large as
// the number of notes sitting unread for one session, so the bound is a guard
// against pathology rather than a working limit — and when it trips, the whole
// set is dropped instead of being evicted entry by entry. Forgetting that a note
// was previewed costs one repeated preview; it cannot cost a message, because the
// note is still in the store and check_messages is still what delivers it.
const maxPreviewedTracked = 512

// chatWatch caches this connection's view of the notifier generations, so the
// steady state costs no I/O, and remembers which notes it has already previewed,
// so an unread one is not pasted onto every subsequent tool result.
//
// The previewed set lives in memory on purpose. It is not a read watermark and
// must never be mistaken for one: the watermark is a column in the store, set
// only by check_messages and session_start. This is presentation state — "have I
// already shown this to the agent on a result it may or may not have seen" — and
// losing it on a reconnect is harmless.
//
// Concurrency: safe for concurrent use; tool calls on one connection can overlap.
type chatWatch struct {
	mu        sync.Mutex
	keys      []string
	gens      []uint64
	lastFull  time.Time
	previewed map[string]bool
}

// unpreviewed returns the notes in pending this connection has not shown yet,
// in order and uncapped. It records NOTHING: marking is markPreviewed's job,
// and the split exists because the two must not be the same step. A note marked
// here but then dropped by the caller's cap would never be previewed again —
// silently un-shown rather than merely deferred, which is a smaller version of
// the bug this whole change is about.
func (w *chatWatch) unpreviewed(pending []tools.Preview) []tools.Preview {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []tools.Preview
	for _, p := range pending {
		if !w.previewed[p.Key] {
			out = append(out, p)
		}
	}
	return out
}

// markPreviewed records the notes actually shown, so they are not pasted onto
// every subsequent tool result.
func (w *chatWatch) markPreviewed(shown []tools.Preview) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.previewed == nil || len(w.previewed)+len(shown) > maxPreviewedTracked {
		w.previewed = make(map[string]bool, len(shown))
	}
	for _, p := range shown {
		w.previewed[p.Key] = true
	}
}

// due reports whether the store should be consulted, and records the decision.
// It returns true when a generation has advanced since the last look (a peer
// wrote something) or when the periodic backstop is due.
func (w *chatWatch) due(keys []string, gens []uint64, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	changed := len(w.keys) != len(keys) || len(w.gens) != len(gens)
	if !changed {
		for i := range keys {
			if w.keys[i] != keys[i] || w.gens[i] != gens[i] {
				changed = true
				break
			}
		}
	}
	backstop := now.Sub(w.lastFull) >= chatFullCheckInterval
	if !changed && !backstop {
		return false
	}
	w.keys = append(w.keys[:0], keys...)
	w.gens = append(w.gens[:0], gens...)
	w.lastFull = now
	return true
}

// invalidate drops the cached baseline so the next call consults the store
// again. Used when a claim filled the per-call cap: more messages are still
// waiting, and leaving them behind the "nothing new" cache would park them
// until the periodic backstop rather than delivering them on the next call.
func (w *chatWatch) invalidate() {
	w.mu.Lock()
	w.lastFull = time.Time{}
	w.mu.Unlock()
}

// reset clears the cached generations, so the next call re-checks the store.
// Called on a workspace re-pin: the new project has a different collab.db, and
// the previous project's counters say nothing about it. The previewed keys go
// with them for the same reason — they name rows in a store this connection has
// stopped reading, and a row id there says nothing about a row id here.
func (w *chatWatch) reset() {
	w.mu.Lock()
	w.keys, w.gens = nil, nil
	w.lastFull = time.Time{}
	w.previewed = nil
	w.mu.Unlock()
}

// messageHint returns a PREVIEW of the messages waiting for this session, or ""
// when there are none or they have all been previewed already. Gated on [collab]
// mailbox. Advisory: it never affects the tool's own result, and every error is
// swallowed — a mailbox problem must not turn a successful tool call into a
// failure.
//
// It claims nothing, and that is the correctness property rather than an
// optimisation. This block is appended to the result of whatever tool the agent
// happened to call, and no client owes plumb any promise to show the model text
// it did not ask for. A harness that runs plumb's tools inside a sandboxed
// program discards the whole result; when this path claimed, that discarded the
// message with it — check_messages went on to report an empty mailbox, correctly,
// because the row really had been marked read. Reproduced end-to-end, and the
// reason the watermark now belongs to check_messages and session_start alone.
//
// What is left here is worth keeping: on a client that does surface the text,
// the agent learns about the message on its very next call with no round trip.
// It just is not evidence of anything, so it is rendered as a preview and
// suppressed per note so it cannot become a banner on every result.
func (s *connSession) messageHint(ctx context.Context) string {
	if s.chatWatch == nil {
		return ""
	}
	ccfg := s.collabConfig()
	if !ccfg.Mailbox {
		return ""
	}
	inbox := s.inbox()
	keys := inbox.Keys()
	if len(keys) == 0 {
		return ""
	}
	notifier := s.collabPool.notifier()
	if !s.chatWatch.due(keys, notifier.Gens(keys), time.Now()) {
		return "" // provably nothing new since the last check: no query
	}
	// due() is not evidence that a message exists. The 30-second backstop returns
	// true unconditionally, and a generation bump may belong to a same-named peer
	// in a project this session cannot read. Both resolve to "nothing for me", so
	// the common outcome of consulting the store is a claim that matches no rows —
	// ~100-145µs of query planning under the writer lock to answer "no". The probe
	// answers the same question with a LIMIT 1 lookup at ~20-30µs and takes no
	// write lock; the claim runs only once something is actually waiting.
	if !inbox.HasPending(ctx) {
		return ""
	}
	fresh := s.chatWatch.unpreviewed(inbox.Peek(ctx))
	named, next := splitNextNotes(fresh)

	// Cap the bodies, and count what the cap deferred. The count has to come from
	// what is left of THIS slice, not from the whole pending set: notes already
	// previewed are not a backlog, and reporting them as one both lies to the
	// agent and re-invalidates the generation cache on every call, putting a query
	// back on the hot path the cache exists to keep clear.
	more := 0
	if len(named) > tools.MaxPreviewedPerCall {
		more = len(named) - tools.MaxPreviewedPerCall
		named = named[:tools.MaxPreviewedPerCall]
	}
	if len(named) == 0 && len(next) == 0 {
		return "" // everything waiting has already been shown once
	}

	block := tools.RenderMessagePreview(tools.PreviewRows(named), inbox.Policy.ChatBudget(), time.Now())
	block += tools.RenderNextWaiting(len(next))
	if more > 0 {
		s.chatWatch.invalidate() // the remainder must arrive on the next call, not in 30s
		// And the recipient must KNOW the remainder exists — "3 waiting" alone
		// cannot say three-of-three rather than three-of-more, and an agent that
		// goes idle here believes it has seen everything. Peek already returned
		// the claimable set, so this is arithmetic on what we hold; the separate
		// counting query this used to run is gone.
		block += tools.RenderBacklog(more)
	}
	s.chatWatch.markPreviewed(append(named, next...))
	return strings.TrimRight(block, "\n")
}

// splitNextNotes separates notes addressed to this session by NAME from those
// left for "whoever attaches next".
//
// They cannot be previewed the same way. A named note has exactly one possible
// recipient, so showing its body early is free. A "next" note has as many
// candidates as there are sessions here and exactly one winner, decided by the
// atomic claim — so printing its body to each of them invites two agents to act
// on one instruction while the store records a single recipient. The listing in
// workspace_sessions refuses to show "next" notes for this exact reason
// (collab.Store.PendingNotes), and a preview is a listing with the body
// attached. What this path may honestly say is that one is waiting; taking
// delivery, and finding out whether it was yours, is check_messages' job.
func splitNextNotes(pending []tools.Preview) (named, next []tools.Preview) {
	for _, p := range pending {
		if p.Row.Addressee == collab.AddresseeNext {
			next = append(next, p)
		} else {
			named = append(named, p)
		}
	}
	return named, next
}
