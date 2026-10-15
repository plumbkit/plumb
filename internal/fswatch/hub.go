package fswatch

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sync"
)

// Hub shares one OS watcher per root among the daemon's consumers (PLAN-503).
// A polyglot workspace used to run one recursive watcher for its topology index
// and one per language server — three FSEvents streams, or three inotify watch
// sets counted against fs.inotify.max_user_watches, over the same tree. Each
// consumer now subscribes, and the hub opens the watcher on the first
// subscription for a (root, options) pair and closes it on the last
// unsubscribe.
//
// Fan-out never lets one consumer block another: every subscription has its own
// buffer, a full buffer is Lost for that subscription only, and Lost and Failed
// from the OS watcher reach every subscription. Exclusions that differ between
// consumers are a subscription's own filter (Subscribe's exclude); the shared
// watcher carries only the options every subscriber passed alike.
//
// Concurrency: all methods are safe for concurrent use. Opening a watcher (its
// initial walk) happens outside the hub's mutex; a concurrent Subscribe for the
// same key waits for it, one for another key does not.
type Hub struct {
	mu      sync.Mutex
	watches map[hubKey]*sharedWatch
	// open builds the OS watcher; nil means New. A test seam.
	open func(root string, opts Options) (*Watcher, error)
}

type hubKey struct {
	root string
	opts Options
}

// sharedWatch is one OS watcher and its subscriptions.
type sharedWatch struct {
	ready chan struct{} // closed once w (or err) is set
	w     *Watcher
	err   error

	mu     sync.Mutex // guards subs and failed; fan-out sends hold it
	subs   map[*Subscription]struct{}
	failed bool
	done   chan struct{} // closed when the fan-out goroutine returns
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{watches: make(map[hubKey]*sharedWatch)} }

// Subscription is one consumer's view of a shared watcher: the same contract as
// a Watcher's Events, Lost and Failed, filtered by the subscription's own
// exclusion. Close ends it; the last Close of a root closes the OS watcher.
type Subscription struct {
	hub     *Hub
	key     hubKey
	sw      *sharedWatch
	exclude *regexp.Regexp

	events    chan Event
	lost      chan struct{}
	failed    chan struct{}
	failOnce  sync.Once
	closeOnce sync.Once
}

// Events delivers this subscription's changes. It is closed by Close.
func (s *Subscription) Events() <-chan Event { return s.events }

// Lost signals that this subscription missed changes — its own buffer was full,
// or the OS watcher lost some — and the consumer must reconcile. Closed by Close.
func (s *Subscription) Lost() <-chan struct{} { return s.lost }

// Failed is closed, once, when the shared watcher can no longer see every
// change (see Watcher.Failed).
func (s *Subscription) Failed() <-chan struct{} { return s.failed }

// Subscribe joins the shared watcher for root with opts, opening it when this
// is the first subscription. exclude, when non-empty, filters this
// subscription's events beyond opts.ExcludeRegex (build it with
// ExcludeDirsRegex), so consumers with different exclusions still share a
// watcher. A nil hub opens a private watcher, exactly as New would.
func (h *Hub) Subscribe(root string, opts Options, exclude string) (*Subscription, error) {
	var re *regexp.Regexp
	if exclude != "" {
		var err error
		if re, err = regexp.Compile(exclude); err != nil {
			return nil, fmt.Errorf("fswatch: subscription exclude regex: %w", err)
		}
	}
	if h == nil {
		return privateSubscription(root, opts, re)
	}
	key := hubKey{root: filepath.Clean(root), opts: opts}
	for {
		h.mu.Lock()
		sw, ok := h.watches[key]
		if ok && sw.hasFailed() {
			// A watcher that can no longer see every change is not handed to a
			// new subscriber: a consumer that opens again (an LSP wake, a
			// topology re-open) gets a fresh one, as a private watcher did. Its
			// existing subscribers keep it until they leave (Close never deletes
			// a successor from the map).
			delete(h.watches, key)
			ok = false
		}
		if !ok {
			sw = &sharedWatch{ready: make(chan struct{}), subs: make(map[*Subscription]struct{}), done: make(chan struct{})}
			h.watches[key] = sw
			h.mu.Unlock()
			h.openShared(key, sw)
		} else {
			h.mu.Unlock()
		}
		<-sw.ready
		if sw.err != nil {
			return nil, sw.err
		}
		if s, ok := h.join(key, sw, re); ok {
			return s, nil
		}
		// The watcher closed between the lookup and the join (its last
		// subscriber left): look again, which opens a fresh one.
	}
}

// openShared opens sw's OS watcher outside h.mu and starts its fan-out. A
// failed open is removed from the hub, so the next Subscribe retries.
func (h *Hub) openShared(key hubKey, sw *sharedWatch) {
	open := h.open
	if open == nil {
		open = New
	}
	w, err := open(key.root, key.opts)
	if err != nil {
		h.mu.Lock()
		delete(h.watches, key)
		h.mu.Unlock()
		sw.err = err
		close(sw.ready)
		return
	}
	sw.w = w
	close(sw.ready)
	go sw.fanOut()
}

// hasFailed reports whether sw's OS watcher has closed Failed. Caller may hold
// h.mu (sw.mu nests inside it, as in join).
func (sw *sharedWatch) hasFailed() bool {
	select {
	case <-sw.ready:
	default:
		return false // still opening
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.failed
}

// join adds a subscription to a ready, open watcher. It fails when the watcher
// was already closed by its last subscriber.
func (h *Hub) join(key hubKey, sw *sharedWatch, re *regexp.Regexp) (*Subscription, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.watches[key] != sw {
		return nil, false
	}
	s := &Subscription{
		hub: h, key: key, sw: sw, exclude: re,
		events: make(chan Event, eventBuffer),
		lost:   make(chan struct{}, 1),
		failed: make(chan struct{}),
	}
	sw.mu.Lock()
	sw.subs[s] = struct{}{}
	if sw.failed {
		s.fail()
	}
	sw.mu.Unlock()
	return s, true
}

// fanOut copies the OS watcher's signals to every subscription until the
// watcher closes.
func (sw *sharedWatch) fanOut() {
	defer close(sw.done)
	events, lost, failed := sw.w.Events(), sw.w.Lost(), sw.w.Failed()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			sw.each(func(s *Subscription) { s.deliver(ev) })
		case _, ok := <-lost:
			if !ok {
				return
			}
			sw.each(func(s *Subscription) { s.signalLost() })
		case <-failed:
			failed = nil // closed for good; stop selecting it
			sw.mu.Lock()
			sw.failed = true
			sw.mu.Unlock()
			sw.each(func(s *Subscription) { s.fail() })
		}
	}
}

// each runs fn for every current subscription under sw.mu, so it never sends
// on a subscription that Close has already removed and closed.
func (sw *sharedWatch) each(fn func(*Subscription)) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	for s := range sw.subs {
		fn(s)
	}
}

// deliver hands ev to the subscription without blocking the fan-out; a full
// buffer is Lost for this subscription only.
func (s *Subscription) deliver(ev Event) {
	if s.exclude != nil && (s.exclude.MatchString(ev.Path) || s.exclude.MatchString(filepath.Base(ev.Path))) {
		return
	}
	select {
	case s.events <- ev:
	default:
		s.signalLost()
	}
}

func (s *Subscription) signalLost() {
	select {
	case s.lost <- struct{}{}:
	default: // one pending signal is enough
	}
}

func (s *Subscription) fail() { s.failOnce.Do(func() { close(s.failed) }) }

// Close ends the subscription and closes its Events and Lost. The last Close
// for a root closes the OS watcher. Safe to call more than once.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		if s.hub == nil { // a private subscription owns its watcher
			s.sw.w.Close() // ends the fan-out: Events closes
			<-s.sw.done
			s.sw.mu.Lock()
			delete(s.sw.subs, s)
			close(s.events)
			close(s.lost)
			s.sw.mu.Unlock()
			return
		}
		s.hub.mu.Lock()
		sw := s.sw
		sw.mu.Lock()
		delete(sw.subs, s)
		close(s.events)
		close(s.lost)
		last := len(sw.subs) == 0
		sw.mu.Unlock()
		if last && s.hub.watches[s.key] == sw {
			delete(s.hub.watches, s.key)
		}
		s.hub.mu.Unlock()
		if last {
			sw.w.Close() // ends the fan-out: Events closes
			<-sw.done
		}
	})
}

// privateSubscription is Subscribe without a hub: a watcher of its own, with
// the same fan-out and filter, closed by the subscription's Close.
func privateSubscription(root string, opts Options, re *regexp.Regexp) (*Subscription, error) {
	w, err := New(root, opts)
	if err != nil {
		return nil, err
	}
	sw := &sharedWatch{ready: make(chan struct{}), w: w, subs: make(map[*Subscription]struct{}), done: make(chan struct{})}
	close(sw.ready)
	s := &Subscription{
		sw: sw, exclude: re,
		events: make(chan Event, eventBuffer),
		lost:   make(chan struct{}, 1),
		failed: make(chan struct{}),
	}
	sw.subs[s] = struct{}{}
	go sw.fanOut() // only Close ends the watcher, and Close closes the channels
	return s, nil
}

// Subscriptions reports how many live subscriptions the hub's watchers have (a
// test and diagnostics seam).
func (h *Hub) Subscriptions() int {
	h.mu.Lock()
	watches := make([]*sharedWatch, 0, len(h.watches))
	for _, sw := range h.watches {
		watches = append(watches, sw)
	}
	h.mu.Unlock()
	n := 0
	for _, sw := range watches {
		sw.mu.Lock()
		n += len(sw.subs)
		sw.mu.Unlock()
	}
	return n
}

// Watches reports how many OS watchers the hub holds open (a test and
// diagnostics seam).
func (h *Hub) Watches() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.watches)
}
