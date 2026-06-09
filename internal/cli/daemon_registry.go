package cli

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
)

// connHandle is the per-connection state the registry tracks: the cancel func
// (idle reaper / shutdown), and the session's workspace + project-config reload
// hook (the reload-project control command).
type connHandle struct {
	cancel        context.CancelFunc
	workspace     func() string
	reloadProject func()
}

// connRegistry tracks live MCP connections so the idle reaper can cancel them
// and the control socket can target a per-workspace config reload.
// Concurrency: all methods are safe for concurrent use.
type connRegistry struct {
	mu    sync.Mutex
	conns map[string]connHandle // sessID → handle
}

func newConnRegistry() *connRegistry {
	return &connRegistry{conns: make(map[string]connHandle)}
}

func (r *connRegistry) add(sessID string, h connHandle) {
	r.mu.Lock()
	r.conns[sessID] = h
	r.mu.Unlock()
}

// reloadProject re-applies the project config to every live session pinned to
// workspace ws — and only those — so a per-workspace config change takes effect
// immediately for that project and never touches a session in another. The
// reload hooks are collected under the lock and invoked outside it, since
// applyProjectConfig may take per-session locks of its own.
func (r *connRegistry) reloadProject(ws string) {
	target := filepath.Clean(ws)
	r.mu.Lock()
	var hits []func()
	for _, h := range r.conns {
		if h.workspace == nil || h.reloadProject == nil {
			continue
		}
		if filepath.Clean(h.workspace()) == target {
			hits = append(hits, h.reloadProject)
		}
	}
	r.mu.Unlock()
	for _, fn := range hits {
		fn()
	}
}

func (r *connRegistry) remove(sessID string) {
	r.mu.Lock()
	delete(r.conns, sessID)
	r.mu.Unlock()
}

// evictIdle cancels connections whose sessions have been idle longer than ttl.
// A zero or negative ttl is a no-op (eviction disabled).
func (r *connRegistry) evictIdle(ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	infos, err := session.List()
	if err != nil || len(infos) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, info := range infos {
		lastSeen := info.LastSeenAt
		if lastSeen.IsZero() {
			lastSeen = info.StartedAt
		}
		if time.Since(lastSeen) < ttl {
			continue
		}
		if h, ok := r.conns[info.ID]; ok && h.cancel != nil {
			slog.Info("daemon: evicting idle session", "session", info.Name, "last_seen", lastSeen)
			h.cancel()
		}
	}
}

// reaperInterval is how often the idle-session reaper runs.
const reaperInterval = 5 * time.Minute

func runIdleReaper(ctx context.Context, store *config.Store, registry *connRegistry, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			ttlMin := store.Current().Session.EvictionTTLMinutes
			registry.evictIdle(time.Duration(ttlMin) * time.Minute)
		}
	}
}
