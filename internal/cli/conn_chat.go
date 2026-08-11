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
// design problem: a SQLite read on every read_file would be a real regression.
//
// The answer is the daemon-wide in-process notifier. A send bumps a generation
// counter for the recipient; this path compares its cached counters and touches
// the database only when one has moved. The steady state — no mail — is a map
// lookup under one mutex and nothing else.
//
// The counters are a fast path, not the truth. They reset when the daemon
// restarts, and a message may have been written by a previous daemon, so a
// periodic full check backstops them: a missed bump costs delivery latency,
// never a message.

// chatFullCheckInterval is how often the database is consulted even when the
// notifier reports nothing new. It bounds the worst-case delivery delay after a
// daemon restart (which zeroes the counters) without putting a query on the hot
// path in the common case.
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
