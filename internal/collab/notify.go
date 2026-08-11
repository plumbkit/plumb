package collab

import (
	"context"
	"path/filepath"
	"slices"
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
// the truth. Counters start at zero after a daemon restart, and an entry may be
// evicted to keep the map bounded, so callers must still fall back to a periodic
// database check; a missed bump costs latency, never a lost message.
//
// Concurrency: safe for concurrent use by any number of sessions.
type Notifier struct {
	mu sync.Mutex
	// seq is a single monotonic clock stamped into every entry, rather than a
	// per-key count. That is what makes evicting an entry safe: a value written
	// after a caller's snapshot is ALWAYS greater than the value it snapshotted,
	// whether or not the key was dropped and recreated in between. A per-key
	// counter would restart at 1 after eviction and could read as "older than the
	// baseline", suppressing a delivery the caller was waiting for.
	seq     uint64
	gen     map[string]uint64
	waiters map[string][]chan struct{}
}

// maxTrackedKeys bounds the generation map. Every distinct addressee ever sent
// to leaves an entry behind and nothing ever removed one, so on a daemon that
// runs for weeks the map only grew — and since a sender may address any name it
// invents, nothing but a ceiling bounds it. 4096 is far past any plausible number
// of live agents, which makes the eviction below a safety valve rather than part
// of steady-state behaviour.
const maxTrackedKeys = 4096

// NewNotifier returns a ready notifier.
func NewNotifier() *Notifier {
	return &Notifier{gen: make(map[string]uint64), waiters: make(map[string][]chan struct{})}
}

// NotifyKey derives the wake-up key an addressee is watched under. A session
// NAME is a daemon-wide address — cross-project mail is delivered by name — so a
// name is its own key. AddresseeNext is not: every workspace has a "next
// arrival", so one shared key means a note left in ANY project wakes every
// connection in the daemon, including sessions pinned to unrelated repositories,
// each of which then runs a needless SQLite claim to discover the note was never
// theirs. Scoping it to the workspace keeps that wake-up inside the project it
// belongs to.
//
// This is the in-process signal only. The addressee STORED in collab.db stays
// AddresseeNext — the key is not an address.
func NotifyKey(workspace, addressee string) string {
	if addressee != AddresseeNext || workspace == "" {
		return addressee
	}
	return AddresseeNext + "\x00" + filepath.Clean(workspace)
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
		n.seq++
		n.gen[k] = n.seq
		for _, ch := range n.waiters[k] {
			select {
			case ch <- struct{}{}:
			default: // already signalled; the waiter will see the generation change
			}
		}
		delete(n.waiters, k)
	}
	n.evictLocked()
}

// evictLocked drops the least recently bumped entries once the map exceeds its
// ceiling, halving it so the scan is amortised over the next few thousand sends.
// The stamp is the clock, so the oldest entries are simply the lowest values. An
// entry with a parked waiter is kept: dropping it is harmless (the waiter is
// signalled by its channel, and the monotonic stamp keeps a recreated entry
// ahead of any snapshot) but it would cost that session a pointless query.
func (n *Notifier) evictLocked() {
	if len(n.gen) <= maxTrackedKeys {
		return
	}
	stamps := make([]uint64, 0, len(n.gen))
	for _, v := range n.gen {
		stamps = append(stamps, v)
	}
	slices.Sort(stamps)
	cutoff := stamps[len(stamps)-maxTrackedKeys/2]
	for k, v := range n.gen {
		if v < cutoff && len(n.waiters[k]) == 0 {
			delete(n.gen, k)
		}
	}
}

// Gen returns the current generation for a recipient key. A caller compares it
// with the value it last acted on: unchanged means there is provably nothing new
// since then, so no query is needed. The value is a monotonic stamp, not a
// message count — only its movement carries meaning.
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
