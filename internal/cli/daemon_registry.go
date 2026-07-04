package cli

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// connHandle is the per-connection state the registry tracks: the cancel func
// (idle reaper / shutdown), and the session's workspace + project-config reload
// hook (the reload-project control command).
type connHandle struct {
	cancel        context.CancelFunc
	workspace     func() string
	reloadProject func()
	// summarise generates this session's episodic summary; invoked by the idle
	// reaper once per idle spell. nil when episodic summaries are unavailable.
	summarise func()
}

// connRegistry tracks live MCP connections so the idle reaper can cancel them
// and the control socket can target a per-workspace config reload.
// Concurrency: all methods are safe for concurrent use.
type connRegistry struct {
	mu    sync.Mutex
	conns map[string]connHandle // sessID → handle
	// summarisedAt records the last-seen time a session was summarised at, so the
	// reaper summarises at most once per idle spell (re-arming after new activity).
	summarisedAt map[string]time.Time
}

func newConnRegistry() *connRegistry {
	return &connRegistry{
		conns:        make(map[string]connHandle),
		summarisedAt: make(map[string]time.Time),
	}
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
	delete(r.summarisedAt, sessID)
	r.mu.Unlock()
}

// summariseIdle generates an episodic summary for each session idle longer than
// threshold, at most once per idle spell (re-arming when the session is active
// again). Summaries run in their own goroutines so a slow one never stalls the
// reaper. A zero/negative threshold is a no-op.
func (r *connRegistry) summariseIdle(threshold time.Duration) {
	if threshold <= 0 {
		return
	}
	infos, err := session.List()
	if err != nil || len(infos) == 0 {
		return
	}
	r.mu.Lock()
	var run []func()
	for _, info := range infos {
		lastSeen := info.LastSeenAt
		if lastSeen.IsZero() {
			lastSeen = info.StartedAt
		}
		if time.Since(lastSeen) < threshold {
			continue
		}
		h, ok := r.conns[info.ID]
		if !ok || h.summarise == nil || !lastSeen.After(r.summarisedAt[info.ID]) {
			continue
		}
		r.summarisedAt[info.ID] = lastSeen
		run = append(run, h.summarise)
	}
	r.mu.Unlock()
	for _, fn := range run {
		go fn()
	}
}

// evictIdle cancels connections whose sessions have been idle longer than ttl.
// A zero or negative ttl is a no-op (eviction disabled). It also summarises an
// idle session that has not yet been summarised this spell BEFORE cancelling it,
// so a session whose eviction TTL is shorter than the episodic-summary threshold
// still gets a "Last session" summary (the summary goroutine reads only the
// atomic view + the daemon-global stats writer, so it completes after teardown).
func (r *connRegistry) evictIdle(ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	infos, err := session.List()
	if err != nil || len(infos) == 0 {
		return
	}
	r.mu.Lock()
	var summaries, cancels []func()
	for _, info := range infos {
		lastSeen := info.LastSeenAt
		if lastSeen.IsZero() {
			lastSeen = info.StartedAt
		}
		if time.Since(lastSeen) < ttl {
			continue
		}
		h, ok := r.conns[info.ID]
		if !ok {
			continue
		}
		if h.summarise != nil && lastSeen.After(r.summarisedAt[info.ID]) {
			r.summarisedAt[info.ID] = lastSeen
			summaries = append(summaries, h.summarise)
		}
		if h.cancel != nil {
			slog.Info("daemon: evicting idle session", "session", info.Name, "last_seen", lastSeen)
			cancels = append(cancels, h.cancel)
		}
	}
	r.mu.Unlock()
	for _, fn := range summaries {
		go fn()
	}
	for _, fn := range cancels {
		fn()
	}
}

// pruneCollab deletes expired intents/notes across every open collab store on
// the reaper tick. Reads filter expired rows regardless, so this is a space
// reclaim, not a correctness requirement. Best-effort and nil-safe.
func pruneCollab(ctx context.Context, collabPool *collabPool) {
	if collabPool == nil {
		return
	}
	now := time.Now()
	for _, s := range collabPool.openStores() {
		if _, err := s.Prune(ctx, now); err != nil {
			slog.Debug("collab: prune failed", "workspace", s.Workspace(), "err", err)
		}
	}
}

// reaperInterval is how often the idle-session reaper runs.
const reaperInterval = 5 * time.Minute

func runIdleReaper(ctx context.Context, store *config.Store, registry *connRegistry, sessState *sessionstate.Store, collabPool *collabPool, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			cur := store.Current()
			pruneSessionState(sessState, cur.Session.PersistStateTTLMinutes)
			pruneCollab(ctx, collabPool)
			// Always run summariseIdle (no global gate): the per-session closure
			// re-checks the project [memory] config, so a per-project episodic
			// opt-in is honoured even when the global default is off. The threshold
			// is global-resolved (Memory.IdleSummaryMinutes, falling back to
			// Session.IdleThresholdMinutes).
			thr := cur.Memory.IdleSummaryMinutes
			if thr == 0 {
				thr = cur.Session.IdleThresholdMinutes
			}
			registry.summariseIdle(time.Duration(thr) * time.Minute)
			registry.evictIdle(time.Duration(cur.Session.EvictionTTLMinutes) * time.Minute)
		}
	}
}
