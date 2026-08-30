package cli

// conn_agent_shard.go — the per-logical-agent copy of the mutable facts a shared
// connection must not let one agent reset for another (PLAN-286 §3).
//
// State is keyed per MCP connection today. On a SINGLE-agent connection that is
// the right key: the connection IS the agent. On a SHARED connection — a
// multiplexing client running several logical agents over one plumb serve — the
// pin, read/write trackers, undo store, rate budget and language must be keyed
// per (connection, logical-agent) so a peer's session_start or tracker Reset
// cannot clobber another agent. The per-call logical-agent identity rides the
// tools/call ctx (see internal/mcp), so every accessor below takes ctx and
// falls back to the connection's own sessionView/trackers when the connection is
// not shared — which keeps the single-agent hot path byte-identical.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// agentShard is one logical agent's copy of the mutable facts. It exists only on
// a shared connection; a nil *agentShard means "use the connection's sessionView
// and tracker fields". The scalar fields (root/language/policy/pinOrigin) are
// guarded by mu — the re-pin path mutates them, readers resolve them lock-free
// via the shard pointer. The trackers/limiter/undo carry their own internal
// locks and are never swapped after creation.
type agentShard struct {
	id       string // the logical-agent identity this shard is keyed on
	mu       sync.RWMutex
	root     string
	language string
	policy   *tools.PathPolicy
	// pinOrigin mirrors sessionView.pinOrigin so the per-agent sticky-pin guard
	// (repinAgent) can apply the same session_start-vs-roots distinction.
	pinOrigin sessionstate.PinSource
	// selfPinned records that THIS agent successfully re-pinned its shard to a
	// root of its own choosing (repinAgent, changed=true). A shard that has
	// never done so is where the CONNECTION seeded it, and follows the
	// connection when it moves (followConnectionShards, PLAN-398): the stale
	// sticky seed otherwise refused the agent's next legitimate call — the one
	// defect where a fresh agent's identical request succeeded. Set only under
	// sh.mu; never cleared.
	selfPinned bool

	readTracker  *tools.ReadTracker
	writeTracker *tools.WriteTracker
	undoStore    *tools.UndoStore
	writeLimiter *tools.RateLimiter
}

// shardFor resolves the per-agent shard for the logical agent carried in ctx.
// It returns nil — signalling "use the connection's own state" — when the
// connection is not shared, or when ctx carries no logical-agent identity on a
// shared connection: an unattributable call never inherits a peer's shard
// (PLAN-394), it resolves against the connection, and OnToolRefusal has already
// refused its state-changing half. On a shared connection the shard is created
// lazily on first use and seeded from the connection's current pin/language,
// with fresh trackers/limiter/undo so each agent starts isolated from its peers.
func (s *connSession) shardFor(ctx context.Context) *agentShard {
	id := mcp.LogicalAgentFromCtx(ctx)
	// sharedWith counts the CALLER, not just the identities already committed: an
	// agent declaring one the connection has not recorded yet is still a second
	// agent, and must be routed to its own shard without that routing being
	// written down. See sharedWith for why the commitment waits for success.
	if !s.logicalAgents.sharedWith(id) {
		return nil
	}
	if id == "" {
		// PLAN-394: on a shared connection an anonymous call has no trustworthy
		// attribution. The attach-time session_id is whichever agent attached
		// LAST — inheriting it routed this call onto that peer's shard: the
		// peer's workspace, its boundary policy, its trackers — so after a
		// peer's force-pin elsewhere, an unattributable read resolved to the
		// peer's project. Fail closed to the connection-level state instead.
		return nil
	}
	s.shardsMu.Lock()
	defer s.shardsMu.Unlock()
	if sh, ok := s.shards[id]; ok {
		return sh
	}
	v := s.view()
	sh := &agentShard{
		id:           id,
		root:         v.acquiredRoot,
		language:     v.acquiredLanguage,
		readTracker:  tools.NewReadTracker(),
		writeTracker: tools.NewWriteTracker(),
		undoStore:    tools.NewUndoStore(),
		writeLimiter: tools.NewRateLimiter(s.store.Current().Edits.RateLimitPerMinute, time.Minute),
		pinOrigin:    v.pinOrigin,
	}
	// Restore a pin this agent persisted before the restart (PLAN-286): it takes
	// precedence over the connection's current pin. A pin that no longer verifies
	// is ignored, so the shard keeps the connection's root rather than resurrecting
	// a deleted or widened one.
	if root, language, origin, ok := s.loadPinForAgent(id); ok {
		if resolved, _, intact := s.restoreRootIntact(root); intact {
			sh.root = resolved
			sh.language = language
			sh.pinOrigin = origin
		}
	}
	sh.policy = s.buildAgentPolicy(sh.root, sh.language)
	// Mirror every strict-mode read to the durable store under (proxy, agent),
	// so a shared connection's per-agent reads survive a daemon restart (2e).
	sh.readTracker.SetPersistSink(s.persistReadShard(sh))
	s.rehydrateReadsForAgent(sh, sh.root)
	if s.shards == nil {
		s.shards = make(map[string]*agentShard)
	}
	s.shards[id] = sh
	return sh
}

// repinShard resolves the shard a re-pin may mutate: only an explicit per-call
// _meta identity on a shared connection. The attachID fallback is deliberately
// NOT used here — a roots-list or serve-proxy-replay pin is unattributable and
// stays on the connection's sessionView, so it can never move an agent's pin.
func (s *connSession) repinShard(ctx context.Context) *agentShard {
	if mcp.LogicalAgentFromCtx(ctx) == "" {
		return nil
	}
	return s.shardFor(ctx)
}

// buildAgentPolicy builds a PathPolicy for a (root, language) pair using the
// connection's shared config blocks (extra roots, read roots, allow-dirs,
// dep-roots, pin provenance). The sessionView is copied and its root/language
// overridden, so buildPathPolicy's many config reads stay unchanged while the
// two per-agent facts come from the shard.
func (s *connSession) buildAgentPolicy(root, language string) *tools.PathPolicy {
	v := s.view()
	v.acquiredRoot = root
	v.acquiredLanguage = language
	return s.buildPathPolicy(&v)
}

// workspaceFor returns the workspace pinned for the logical agent in ctx, falling
// back to the connection's pin when the connection is not shared (or the call is
// unattributed). workspace() stays the ctx-less default for background goroutines.
func (s *connSession) workspaceFor(ctx context.Context) string {
	if sh := s.shardFor(ctx); sh != nil {
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		return sh.root
	}
	return s.workspace()
}

// policyFor returns the PathPolicy for the logical agent in ctx, falling back
// to the connection's policy when not shared.
func (s *connSession) policyFor(ctx context.Context) *tools.PathPolicy {
	if sh := s.shardFor(ctx); sh != nil {
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		return sh.policy
	}
	return s.boundaryPolicy()
}

// readTrackerFor returns the read tracker for the logical agent in ctx, falling
// back to the connection's tracker when not shared.
func (s *connSession) readTrackerFor(ctx context.Context) *tools.ReadTracker {
	if sh := s.shardFor(ctx); sh != nil {
		return sh.readTracker
	}
	return s.readTracker
}

// writeTrackerFor returns the write tracker for the logical agent in ctx,
// falling back to the connection's tracker when not shared.
func (s *connSession) writeTrackerFor(ctx context.Context) *tools.WriteTracker {
	if sh := s.shardFor(ctx); sh != nil {
		return sh.writeTracker
	}
	return s.writeTracker
}

// undoStoreFor returns the undo store for the logical agent in ctx, falling back
// to the connection's store when not shared.
func (s *connSession) undoStoreFor(ctx context.Context) *tools.UndoStore {
	if sh := s.shardFor(ctx); sh != nil {
		return sh.undoStore
	}
	return s.undoStore
}

// rateLimiterFor returns the write rate limiter for the logical agent in ctx,
// falling back to the connection's limiter when not shared.
func (s *connSession) rateLimiterFor(ctx context.Context) *tools.RateLimiter {
	if sh := s.shardFor(ctx); sh != nil {
		return sh.writeLimiter
	}
	return s.writeLimiter
}

// repinAgent points the logical agent in ctx at a new workspace. It is the
// per-agent half of the re-pin: on a shared connection the connection-level
// attachOrRepinTo machinery stays put, and only this agent's shard moves — so a
// peer agent's pin is never reset. Returns changed=false when the shard was not
// moved (a no-op re-pin to the same root) and refused!=nil when the per-agent
// sticky-pin guard declined.
//
// The sticky guard is INVERTED from the connection-level one: per-agent, refuse
// only a SAME-agent non-forced re-pin away from an explicit pin. A DIFFERENT
// agent's re-pin lands on its own shard, which is the actual issue #182 fix — the
// connection-level guard refused a second agent's re-pin outright, but the right
// behaviour on a shared connection is isolation, not refusal.
//
// A refusal leaves ONE trace: a Warn in the daemon log. Not a session health
// note — deliberately, and not for lack of trying. Health is a single field per
// session, and on the very connection this feature exists for it is rewritten by
// the next peer that declares an identity (markSharedConnectionDetected fires on
// every declaration). A note whose lifetime is "until the next peer call" is
// worse than none: it reads as durable, decays to noise, and would need a heal
// keyed per agent on a field that has no room for one. Nor "blocked" — one agent
// asking for a project of its own is a scoping question about that agent, not the
// connection being unusable, and flagging it would raise a dashboard alert
// against the coordinator for a peer's call. The log line is greppable, carries
// the agent id and both roots, and does not expire.
func (s *connSession) repinAgent(ctx context.Context, root, language string, origin sessionstate.PinSource, force bool) (changed bool, refused error) {
	sh := s.repinShard(ctx)
	if sh == nil {
		return false, nil
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	prev := sh.root
	if !force && prev != "" && root != prev && sh.pinOrigin == sessionstate.PinSourceSessionStart {
		refused = fmt.Errorf("refusing to re-pin logical agent %q from %s to %s: this agent's pin was set by an explicit session_start and is sticky — issue #182. To switch this agent's project, call session_start again with force: true; to run several agents over one connection, each must identify itself (session_start.session_id or per-call _meta)", mcp.LogicalAgentFromCtx(ctx), prev, root)
		// Leave a trace on this past-vulnerability surface: the connection-level
		// guard has always logged a refused steal, and a refused cross-workspace
		// drift on a SHARED connection is exactly the event an operator needs to
		// find afterwards. The daemon log is the whole trace, deliberately — see
		// the note on repinAgent.
		s.log().Warn("daemon: per-agent session_start re-pin refused — this agent's pin is sticky (issue #182)",
			"agent", sh.id, "pinned", prev, "requested", root, "remedy", repinStickyRemedy)
		return false, refused
	}
	if root == prev && language == sh.language {
		return false, nil
	}
	changed = true
	// The agent has CHOSEN this root (even back to the seeded one, via a
	// deliberate re-pin): from here the shard no longer follows the connection
	// (PLAN-398).
	sh.selfPinned = true
	sh.root = root
	sh.language = language
	sh.pinOrigin = origin
	sh.policy = s.buildAgentPolicy(root, language)
	sh.readTracker.Reset()
	sh.writeTracker.Reset()
	sh.undoStore.Reset()
	s.rehydrateReadsForAgent(sh, root)
	s.persistPinForAgent(sh, root, language, origin)
	return changed, nil
}

// followConnectionShards re-seeds every shard that never chose a workspace of
// its own (!selfPinned) from the connection's NEW pin, after the connection
// itself moved away from prevRoot. A shard is seeded from the connection pin at
// first use — shardFor caches it BEFORE repinAgent can refuse, so one refused
// ask left the agent cached at a root whose sticky seed then refused the
// agent's next, entirely legitimate call (the exact PLAN-398 reproduction),
// while a fresh agent asking the same thing succeeded: the fresh shard seeded
// from the CURRENT pin, the stale one had not followed. Re-seeding here restores
// the invariant "a seeded shard sits where the connection sits" without
// touching shards whose agent deliberately pinned elsewhere — per-agent
// isolation means the connection's move cannot drag an agent that chose its own
// root. Runs OUTSIDE the connection mutate lane, in the documented lock order
// (shardsMu before sh.mu, s.mu innermost), so the per-tool-call hot path's lock
// pattern is unchanged; the writes mirror repinAgent's success path, held under
// one sh.mu acquisition each.
func (s *connSession) followConnectionShards(prevRoot string) {
	if prevRoot == "" {
		return
	}
	v := s.view()
	if v.acquiredRoot == "" || v.acquiredRoot == prevRoot {
		return
	}
	s.shardsMu.Lock()
	defer s.shardsMu.Unlock()
	for _, sh := range s.shards {
		sh.mu.Lock()
		if sh.selfPinned || sh.root != prevRoot {
			sh.mu.Unlock()
			continue
		}
		sh.root = v.acquiredRoot
		sh.language = v.acquiredLanguage
		sh.pinOrigin = v.pinOrigin
		sh.policy = s.buildAgentPolicy(sh.root, sh.language)
		sh.readTracker.Reset()
		sh.writeTracker.Reset()
		sh.undoStore.Reset()
		// Copy what the calls below need while the lock is still held.
		// shardsMu does not exclude repinAgent — that takes sh.mu alone — so
		// reading sh.root/sh.language after the unlock would race a concurrent
		// per-agent re-pin and could persist a root this shard no longer has.
		// Same rule persistReadShard states: the shard's root is read under sh.mu.
		root, language := sh.root, sh.language
		sh.mu.Unlock()
		s.rehydrateReadsForAgent(sh, root)
		s.persistPinForAgent(sh, root, language, v.pinOrigin)
	}
}

// persistReadShard mirrors a per-agent recorded read to the durable store, keyed
// by (proxy session ID, logical-agent ID, workspace) so a shared connection's
// per-agent reads survive a daemon restart. The shard's root is read under
// sh.mu, since repinAgent may move it concurrently with a tool call.
func (s *connSession) persistReadShard(sh *agentShard) func(path string, mtime time.Time, sha string) {
	return func(path string, mtime time.Time, sha string) {
		v := s.view()
		if s.sessionState == nil || !v.session.PersistState || v.proxySessionID == "" {
			return
		}
		sh.mu.RLock()
		root := sh.root
		sh.mu.RUnlock()
		if root == "" {
			return
		}
		if err := s.sessionState.UpsertReadForAgent(v.proxySessionID, sh.id, root, path, mtime, sha); err != nil {
			s.log().Debug("daemon: persist agent read failed", "err", err)
		}
	}
}

// rehydrateReadsForAgent loads the persisted reads for (proxyID, agentID, root)
// into the shard's read tracker. Called from the shard-creation lane and from
// repinAgent (which holds sh.mu), hence the explicit root arg — the caller
// guarantees no concurrent shard mutation, so sh.root is not re-locked here.
func (s *connSession) rehydrateReadsForAgent(sh *agentShard, root string) {
	v := s.view()
	if s.sessionState == nil || !v.session.PersistState || v.proxySessionID == "" || root == "" {
		return
	}
	recs, err := s.sessionState.LoadReadsForAgent(v.proxySessionID, sh.id, root)
	if err != nil {
		s.log().Debug("daemon: rehydrate agent reads failed", "err", err)
		return
	}
	if len(recs) == 0 {
		return
	}
	out := make([]tools.ReadRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, tools.ReadRecord{Path: r.Path, Mtime: r.Mtime, SHA: r.SHA})
	}
	sh.readTracker.Hydrate(out)
	s.log().Info("daemon: rehydrated per-agent read-tracking", "agent", sh.id, "root", root, "count", len(out))
}

// persistPinForAgent records the logical agent's pin under (proxy session,
// agent), so a shared connection's per-agent workspace survives a daemon restart
// (PLAN-286). Mirrors persistPin, scoped to the agent.
func (s *connSession) persistPinForAgent(sh *agentShard, root, language string, origin sessionstate.PinSource) {
	if origin == sessionstate.PinSourceUnknown {
		return
	}
	v := s.view()
	if s.sessionState == nil || !v.session.PersistState || v.proxySessionID == "" || root == "" {
		return
	}
	if err := s.sessionState.UpsertPinForAgent(v.proxySessionID, sh.id, root, language, origin); err != nil {
		s.log().Debug("daemon: persist agent pin failed", "err", err)
	}
}

// loadPinForAgent returns the pin a logical agent persisted under (proxy session,
// agent). ok=false when nothing is recorded or persistence is disabled.
func (s *connSession) loadPinForAgent(id string) (root, language string, origin sessionstate.PinSource, ok bool) {
	v := s.view()
	if s.sessionState == nil || !v.session.PersistState || v.proxySessionID == "" {
		return "", "", sessionstate.PinSourceUnknown, false
	}
	root, language, origin, ok, err := s.sessionState.LoadPinForAgent(v.proxySessionID, id)
	if err != nil || !ok || root == "" {
		return "", "", sessionstate.PinSourceUnknown, false
	}
	return root, language, origin, true
}
