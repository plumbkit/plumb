package config

import (
	"fmt"
	"log/slog"
	"reflect"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Listener is notified with the freshly-published global base config whenever
// the Store changes. Listeners run synchronously on the goroutine that called
// Reload/set, after the Store's listener lock is released, so a Listener may
// safely call back into Current, Generation, or LoadProject; they are invoked
// in registration (Subscribe) order. A Listener must not block for long and
// must not call Reload re-entrantly (it would deadlock on the publish lock). A
// panicking Listener is recovered and logged; it does not stop delivery to the
// remaining listeners.
type Listener func(Config)

// Store is the daemon-singleton source of truth for the global base config
// (compiled defaults + global config file + environment overrides). It holds
// only the global base; per-workspace merges remain the caller's concern via
// LoadProject(store.Current(), workspace).
//
// Concurrency: all methods are safe for concurrent use. Current and Generation
// are lock-free (atomic loads). The published *Config is never mutated in place
// — set swaps in a fresh pointer — so a reader never observes a torn value.
// publishMu serialises set so concurrent reloads produce an ordered sequence of
// generations and notifications; mu guards the listener registry and the
// last-reloaded timestamp. Callers of Current must not mutate the returned maps
// or slices (they are shared with other readers); clone first if mutation is
// needed (LoadProject already does).
type Store struct {
	cur atomic.Pointer[Config]
	gen atomic.Uint64

	publishMu sync.Mutex // serialises set(): one ordered publish at a time

	mu           sync.Mutex // guards listeners, nextID, lastReloaded
	listeners    map[int]Listener
	nextID       int
	lastReloaded time.Time

	// startup is the config captured at daemon start. RestartNeeded compares the
	// restart-bound subset of Current against it. Immutable after NewStore.
	startup Config
}

// NewStore returns a Store seeded with initial as generation 1.
func NewStore(initial Config) *Store {
	s := &Store{
		listeners:    make(map[int]Listener),
		lastReloaded: time.Now(),
		startup:      cloneConfig(initial),
	}
	cfg := initial
	s.cur.Store(&cfg)
	s.gen.Store(1)
	return s
}

// Current returns the live global base config. Lock-free.
func (s *Store) Current() Config { return *s.cur.Load() }

// Generation returns a monotonic counter that increments on every published
// change. A subscriber can compare it to detect whether the config moved since
// it last looked, without holding any lock.
func (s *Store) Generation() uint64 { return s.gen.Load() }

// LastReloaded returns the time of the most recent published change.
func (s *Store) LastReloaded() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastReloaded
}

// Subscribe registers fn to be called on every subsequent published change and
// returns an unsubscribe function. Call unsubscribe when the subscriber is torn
// down, otherwise the Store retains a dead callback.
func (s *Store) Subscribe(fn Listener) (unsubscribe func()) {
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	s.listeners[id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.listeners, id)
		s.mu.Unlock()
	}
}

// Reload re-reads the global config file and, on success, publishes it to every
// subscriber. A read, parse, or validation failure leaves the current config in
// place and returns the error — the Store never publishes an invalid config.
func (s *Store) Reload() error {
	cfg, err := Load()
	if err != nil {
		return fmt.Errorf("reloading config: %w", err)
	}
	s.set(cfg)
	return nil
}

// RestartNeeded reports whether the live config differs from the daemon's
// startup config in a setting the daemon cannot reload live (see
// RestartSensitiveEqual). True means a restart is required for that change to
// take effect; surfaced by `plumb config show` and the TUI.
func (s *Store) RestartNeeded() bool {
	return !RestartSensitiveEqual(s.startup, s.Current())
}

// RestartSensitiveEqual reports whether a and b agree on every setting the
// daemon cannot apply without a restart: LSP server definitions, cache sizing,
// and log format. Everything else (edits, git, walk, topology, log level, theme,
// workspace, lsp_query, quality) is applied live or on the next attach/session.
func RestartSensitiveEqual(a, b Config) bool {
	return a.LogFormat == b.LogFormat &&
		a.Cache == b.Cache &&
		reflect.DeepEqual(a.LSP, b.LSP)
}

// set publishes cfg: swap the pointer, bump the generation, record the time,
// snapshot the listeners, then notify them outside the listener lock so a
// listener may re-enter Current/LoadProject. Each listener is invoked via
// notifyListener, which recovers a panic so one broken subscriber cannot abort
// notification of the rest. publishMu is held across the whole call so
// concurrent reloads cannot interleave their generations or deliver
// notifications out of order.
func (s *Store) set(cfg Config) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	c := cfg
	s.cur.Store(&c)
	s.gen.Add(1)

	s.mu.Lock()
	s.lastReloaded = time.Now()
	// Snapshot listeners in registration (id) order so notification is
	// deterministic: the daemon-level subscriber (registered first, before any
	// connection) runs before every per-session subscriber. Topology relies on
	// this ordering — the daemon reconciles the shared pool first, then each
	// session re-acquires the now-fresh store.
	ids := make([]int, 0, len(s.listeners))
	for id := range s.listeners {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	subs := make([]Listener, 0, len(ids))
	for _, id := range ids {
		subs = append(subs, s.listeners[id])
	}
	s.mu.Unlock()

	for _, fn := range subs {
		notifyListener(fn, c)
	}
}

// notifyListener invokes fn with cfg, recovering a panic so one broken
// listener cannot abort notification of the remaining subscribers or,
// on the file-watch reload path, permanently stall live config reloading for
// the rest of the daemon's lifetime.
func notifyListener(fn Listener, cfg Config) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("config: listener panic — notification continues",
				"err", r,
				"stack", string(debug.Stack()))
		}
	}()
	fn(cfg)
}
