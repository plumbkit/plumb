package cli

import (
	"log/slog"
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
}

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
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.openGlobalLocked()
}

// getGlobal returns the daemon-level store ONLY when it already exists on disk,
// so delivery and prune paths never create it.
func (p *collabPool) getGlobal() *collab.Store {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.global != nil {
		return p.global
	}
	if !collab.GlobalExists() {
		return nil
	}
	return p.openGlobalLocked()
}

// openGlobalLocked returns the cached global store or opens a new one. Must hold
// p.mu.
func (p *collabPool) openGlobalLocked() *collab.Store {
	if p.global != nil {
		return p.global
	}
	s, err := collab.OpenGlobal()
	if err != nil {
		slog.Warn("collab: open cross-project store", "err", err)
		return nil
	}
	p.global = s
	return s
}

// acquire returns the workspace's collab store, opening (and CREATING collab.db
// on first use) if needed. Returns nil when workspace is empty or the store
// cannot be opened. Only the intents/mailbox write tools should call this.
func (p *collabPool) acquire(workspace string) *collab.Store {
	if workspace == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.openLocked(workspace)
}

// get returns the workspace's collab store ONLY when a collab.db already exists,
// opening (and caching) it if so; otherwise nil. It never creates the database,
// so read/hint/prune paths are safe to call it unconditionally.
func (p *collabPool) get(workspace string) *collab.Store {
	if workspace == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.stores[workspace]; ok {
		return s
	}
	if !collab.Exists(workspace) {
		return nil
	}
	return p.openLocked(workspace)
}

// openLocked returns the cached store or opens a new one. Must hold p.mu.
func (p *collabPool) openLocked(workspace string) *collab.Store {
	if s, ok := p.stores[workspace]; ok {
		return s
	}
	s, err := collab.Open(workspace)
	if err != nil {
		slog.Warn("collab: open store", "workspace", workspace, "err", err)
		return nil
	}
	p.stores[workspace] = s
	return s
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
	for _, s := range p.stores {
		_ = s.Close()
	}
	p.stores = make(map[string]*collab.Store)
	if p.global != nil {
		_ = p.global.Close()
		p.global = nil
	}
}
