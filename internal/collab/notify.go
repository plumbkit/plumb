package collab

import (
	"context"
	"sync"
)

// notify.go holds the daemon-wide, in-process wake-up signal for the mailbox.
//
// It exists for two reasons, one about cost and one about latency.
//
// Cost: message delivery is piggybacked onto EVERY tool result, and the enrich
// hook that does it runs synchronously on the response path. Querying SQLite on
// every read_file to discover there is no mail would be a real regression, so a
// session first checks a per-recipient generation counter — an atomic map read —
// and touches the database only when the counter has moved since it last looked.
//
// Latency: a session that wants a reply needs to park until one arrives rather
// than spin on polling calls. Waiters register a channel against the recipient
// keys they care about and are woken by the next Bump.
//
// Every plumb session lives in one daemon process, including sessions pinned to
// different workspaces, so an in-process signal covers same-project and
// cross-project delivery alike. It is a FAST PATH ONLY — the database remains
// the truth. Counters start at zero after a daemon restart, so callers must
// still fall back to a periodic database check; a missed bump costs latency,
// never a lost message.
//
// Concurrency: safe for concurrent use by any number of sessions.
type Notifier struct {
	mu      sync.Mutex
	gen     map[string]uint64
	waiters map[string][]chan struct{}
}

// NewNotifier returns a ready notifier.
func NewNotifier() *Notifier {
	return &Notifier{gen: make(map[string]uint64), waiters: make(map[string][]chan struct{})}
}

// Bump records that a message was written for each recipient key and wakes any
// session waiting on it. Safe to call on a nil notifier so callers need no guard.
func (n *Notifier) Bump(keys ...string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, k := range keys {
		if k == "" {
			continue
		}
		n.gen[k]++
		for _, ch := range n.waiters[k] {
			select {
			case ch <- struct{}{}:
			default: // already signalled; the waiter will see the generation change
			}
		}
		delete(n.waiters, k)
	}
}

// Gen returns the current generation for a recipient key. A caller compares it
// with the value it last acted on: unchanged means there is provably nothing new
// since then, so no query is needed.
func (n *Notifier) Gen(key string) uint64 {
	if n == nil {
		return 0
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.gen[key]
}

// Gens returns the current generation for several keys at once, in the order
// given, under a single lock acquisition.
func (n *Notifier) Gens(keys []string) []uint64 {
	out := make([]uint64, len(keys))
	if n == nil {
		return out
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for i, k := range keys {
		out[i] = n.gen[k]
	}
	return out
}

// Wait blocks until any key's generation advances past the matching entry in
// since, or ctx is done. It reports whether something arrived. The generation
// snapshot is re-checked under the lock before parking, so a message written
// between the caller's Gens call and its Wait call cannot be missed.
//
// since shorter than keys is treated as zero for the missing entries.
func (n *Notifier) Wait(ctx context.Context, keys []string, since []uint64) bool {
	if n == nil || len(keys) == 0 {
		// Nothing can ever signal this call, so parking on ctx would be a
		// disguised hang. Report 'nothing arrived' immediately instead.
		return false
	}
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	for i, k := range keys {
		var was uint64
		if i < len(since) {
			was = since[i]
		}
		if n.gen[k] > was {
			n.mu.Unlock()
			return true
		}
	}
	for _, k := range keys {
		if k != "" {
			n.waiters[k] = append(n.waiters[k], ch)
		}
	}
	n.mu.Unlock()
	defer n.unregister(keys, ch)

	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// unregister drops ch from every key's waiter list. Bump already clears the list
// it signalled, so this is the timeout path; it is idempotent either way.
func (n *Notifier) unregister(keys []string, ch chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, k := range keys {
		w := n.waiters[k]
		for i, c := range w {
			if c == ch {
				n.waiters[k] = append(w[:i], w[i+1:]...)
				break
			}
		}
		if len(n.waiters[k]) == 0 {
			delete(n.waiters, k)
		}
	}
}
