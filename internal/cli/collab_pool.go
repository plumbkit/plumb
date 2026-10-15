package cli

import (
	"errors"
	"log/slog"
	"os"
	"sync"

	"github.com/plumbkit/plumb/internal/collab"
)

// collabPool manages one collab.Store per workspace root, shared across every
// connection to that workspace (collab.db is a WAL SQLite handle, so sharing
// avoids redundant handles). Stores are opened lazily and live until daemon
// shutdown.
//
// Two open modes enforce the lazy-creation contract from the design: acquire
// opens-or-creates (the write path — share_intent / leave_note — is the only
// thing that should ever materialise a collab.db), while get opens only when a
// collab.db already exists on disk, so read, hint, prune, and session-close
// paths never create one for a workspace that has not used the feature.
//
// Concurrency: all methods are safe for concurrent use.
type collabPool struct {
	mu     sync.Mutex
	stores map[string]*collab.Store
	// global is the daemon-level cross-project store, shared by every workspace
	// rather than keyed by one. Opened by the same two-mode contract as the
	// per-workspace stores.
	global *collab.Store
	// notify is the daemon-wide in-process wake-up signal for message delivery.
	// It lives on the pool because it must be shared by every connection,
	// including connections pinned to different workspaces.
	notify *collab.Notifier
	// flight lets a store be opened without p.mu (PLAN-536): an open runs the
	// schema migration. closed marks the pool shut, so an open that finishes
	// afterwards closes its store.
	flight keyedFlight
	closed bool
	// openStore opens a store (workspace "" with global set: the daemon-level
	// one); nil means collab.Open / collab.OpenGlobal. A test seam.
	openStore func(workspace string, global bool) (*collab.Store, error)
}

// collabGlobalKey is the flight key of the daemon-level store: not a path, so
// it can never collide with a workspace.
const collabGlobalKey = "\x00global"

func newCollabPool() *collabPool {
	return &collabPool{stores: make(map[string]*collab.Store), notify: collab.NewNotifier()}
}

// notifier returns the daemon-wide message notifier. Never nil for a pool built
// by newCollabPool; nil-safe for a zero-value pool in tests.
func (p *collabPool) notifier() *collab.Notifier {
	if p == nil {
		return nil
	}
	return p.notify
}

// acquireGlobal returns the daemon-level cross-project store, CREATING it on
// first use. Only the send path calls this, and only once a message is known to
// cross a project boundary, so a daemon whose sessions never talk across
// projects never materialises the file.
func (p *collabPool) acquireGlobal() *collab.Store {
	if p == nil {
		return nil
	}
	s, err := p.open("", true, true)
	if err != nil {
		slog.Warn("collab: open cross-project store", "err", err)
	}
	return s
}

// getGlobal returns the daemon-level store ONLY when it already exists on disk,
// so delivery and prune paths never create it.
func (p *collabPool) getGlobal() *collab.Store {
	store, err := p.getGlobalResult()
	if err != nil {
		slog.Warn("collab: open cross-project store", "err", err)
	}
	return store
}

// getGlobalResult distinguishes an unused mailbox from a failed open.
func (p *collabPool) getGlobalResult() (*collab.Store, error) {
	if p == nil {
		return nil, errors.New("collab: mailbox pool unavailable")
	}
	return p.open("", true, false)
}

// probeGlobalResult observes only an already pooled handle. A cache miss checks
// absence without opening SQLite, so hook probes never run schema migrations.
func (p *collabPool) probeGlobalResult() (*collab.Store, error) {
	if p == nil || !p.mu.TryLock() {
		return nil, errors.New("collab: mailbox pool unavailable")
	}
	store := p.global
	p.mu.Unlock()
	return probeCachedStore(store, collab.GlobalDBPath())
}

// open returns the cached store for workspace (or, with global, the
// daemon-level one), opening it on a miss WITHOUT p.mu. With create false an
// absent database is not created: (nil, nil) means "never used". A concurrent
// open of the same store waits for the first; other stores never wait.
func (p *collabPool) open(workspace string, global, create bool) (*collab.Store, error) {
	key := workspace
	if global {
		key = collabGlobalKey
	}
	for {
		p.mu.Lock()
		if s := p.cachedLocked(key); s != nil {
			p.mu.Unlock()
			return s, nil
		}
		wait, leader := p.flight.begin(key)
		p.mu.Unlock()
		if !leader {
			<-wait
			continue
		}
		return p.openAsLeader(key, workspace, global, create)
	}
}

// openAsLeader opens key's store without p.mu, publishes it and ends the flight.
// Caller holds key's flight. A panicking open still ends it, so the panic cannot
// wedge every later open of key.
func (p *collabPool) openAsLeader(key, workspace string, global, create bool) (*collab.Store, error) {
	ended := false
	defer func() {
		if !ended {
			p.mu.Lock()
			p.flight.end(key)
			p.mu.Unlock()
		}
	}()
	s, err := p.openUnlocked(workspace, global, create)
	p.mu.Lock()
	p.flight.end(key)
	ended = true
	closed := p.closed
	if s != nil && err == nil && !closed {
		p.publishLocked(key, s)
	}
	p.mu.Unlock()
	if s != nil && closed {
		_ = s.Close()
		return nil, errors.New("collab: mailbox pool closed")
	}
	return s, err
}

func (p *collabPool) cachedLocked(key string) *collab.Store {
	if key == collabGlobalKey {
		return p.global
	}
	return p.stores[key]
}

func (p *collabPool) publishLocked(key string, s *collab.Store) {
	if key == collabGlobalKey {
		p.global = s
		return
	}
	p.stores[key] = s
}

// openUnlocked opens a store, or reports (nil, nil) for an absent database when
// create is false. Runs without p.mu.
func (p *collabPool) openUnlocked(workspace string, global, create bool) (*collab.Store, error) {
	if !create {
		path := collab.DBPath(workspace)
		if global {
			path = collab.GlobalDBPath()
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
	}
	if p.openStore != nil {
		return p.openStore(workspace, global)
	}
	if global {
		return collab.OpenGlobal()
	}
	return collab.Open(workspace)
}

// acquire returns the workspace's collab store, opening (and CREATING collab.db
// on first use) if needed. Returns nil when workspace is empty or the store
// cannot be opened. Only the intents/mailbox write tools should call this.
func (p *collabPool) acquire(workspace string) *collab.Store {
	if workspace == "" {
		return nil
	}
	s, err := p.open(workspace, false, true)
	if err != nil {
		slog.Warn("collab: open store", "workspace", workspace, "err", err)
	}
	return s
}

// get returns the workspace's collab store ONLY when a collab.db already exists,
// opening (and caching) it if so; otherwise nil. It never creates the database,
// so read/hint/prune paths are safe to call it unconditionally.
func (p *collabPool) get(workspace string) *collab.Store {
	store, err := p.getResult(workspace)
	if err != nil {
		slog.Warn("collab: open store", "workspace", workspace, "err", err)
	}
	return store
}

// getResult is get with truthful absence/error reporting for explicit reads.
func (p *collabPool) getResult(workspace string) (*collab.Store, error) {
	if p == nil || workspace == "" {
		return nil, errors.New("collab: mailbox workspace unavailable")
	}
	return p.open(workspace, false, false)
}

// probeResult never waits for a concurrent pool open or opens its own handle.
// Metadata-only absence checks run without the pool mutex. An existing cold
// database is unavailable to hooks until an ordinary daemon read opens it.
func (p *collabPool) probeResult(workspace string) (*collab.Store, error) {
	if p == nil || workspace == "" || !p.mu.TryLock() {
		return nil, errors.New("collab: mailbox pool unavailable")
	}
	store := p.stores[workspace]
	p.mu.Unlock()
	return probeCachedStore(store, collab.DBPath(workspace))
}

func probeCachedStore(store *collab.Store, dbPath string) (*collab.Store, error) {
	if store != nil {
		return store, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return nil, errors.New("collab: mailbox handle not pooled")
}

// openStores returns a snapshot of the currently-open stores, for the reaper's
// prune pass. A store enters the pool only once the workspace has used a collab
// feature this daemon lifetime, so pruning the open set covers every workspace
// with live rows without re-scanning disk.
func (p *collabPool) openStores() []*collab.Store {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*collab.Store, 0, len(p.stores)+1)
	for _, s := range p.stores {
		out = append(out, s)
	}
	if p.global != nil {
		out = append(out, p.global)
	}
	return out
}

// closeAll closes every open store. Called by the daemon on shutdown.
func (p *collabPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, s := range p.stores {
		_ = s.Close()
	}
	p.stores = make(map[string]*collab.Store)
	if p.global != nil {
		_ = p.global.Close()
		p.global = nil
	}
}
