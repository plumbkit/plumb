package cli

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
)

// conn_chat.go — the connection-side delivery of agent-to-agent messages
// ([collab] mailbox): the block appended to a tool result when a peer has
// written to this session.
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
// runs the claim.
//
// The counters are a fast path, not the truth. They reset when the daemon
// restarts, and a message may have been written by a previous daemon, so the
// periodic full check backstops them: a missed bump costs delivery latency,
// never a message.

// chatFullCheckInterval is how often the database is probed even when the
// notifier reports nothing new. It bounds the worst-case delivery delay after a
// daemon restart (which zeroes the counters) without putting a query on the hot
// path in the common case. What it now triggers is the probe, not the claim, so
// the periodic cost of an empty mailbox is one indexed lookup rather than a
// write-locked statement that updates nothing.
const chatFullCheckInterval = 30 * time.Second

// chatWatch caches this connection's view of the notifier generations, so the
// steady state costs no I/O.
//
// Concurrency: safe for concurrent use; tool calls on one connection can overlap.
type chatWatch struct {
	mu        sync.Mutex
	keys      []string
	gens      []uint64
	lastFull  time.Time
	lastCheck time.Time
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
	w.lastCheck = now
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
// the previous project's counters say nothing about it.
func (w *chatWatch) reset() {
	w.mu.Lock()
	w.keys, w.gens = nil, nil
	w.lastFull = time.Time{}
	w.mu.Unlock()
}

// messageHint returns the block of messages waiting for this session, claiming
// them so they are never delivered twice, or "" when there are none. Gated on
// [collab] mailbox. Advisory: it never affects the tool's own result, and every
// error is swallowed — a mailbox problem must not turn a successful tool call
// into a failure.
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
	rows := inbox.Claim(ctx)
	if len(rows) == 0 {
		return ""
	}
	if tools.AtCap(rows) {
		w := s.chatWatch
		w.invalidate() // the remainder must arrive on the next call, not in 30s
	}
	return strings.TrimRight(tools.RenderMessages(rows, inbox.Policy.ChatBudget(), time.Now()), "\n")
}
