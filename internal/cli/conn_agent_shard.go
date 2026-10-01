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
	"github.com/plumbkit/plumb/internal/toolerror"
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
	// prov is how, when and from where the pin this shard resolves against was
	// set, so daemon_info and a boundary refusal describe THIS agent's pin rather
	// than the connection's (#529). It is the provenance of the pin in force: the
	// connection's, copied, while the shard follows it; the agent's own once it
	// moves or confirms one; "restore:…" when it came back from its row. Contested
	// is never stored — it is the connection's displacement history, filled in on
	// read (pinProvenanceFor).
	prov tools.PinProvenance
	// selfPinned records that THIS agent successfully re-pinned its shard to a
	// root of its own choosing (repinAgent, changed=true). A shard that has
	// never done so is where the CONNECTION seeded it, and follows the
	// connection when it moves (followConnectionShards, PLAN-398): the stale
	// sticky seed otherwise refused the agent's next legitimate call — the one
	// defect where a fresh agent's identical request succeeded. Set only under
	// sh.mu; never cleared.
	selfPinned bool
	// restored records that this shard's root came from the agent's OWN
	// persisted pin rather than from the connection's seed. It is a distinct
	// fact from selfPinned, which is in-memory only: after a daemon restart the
	// shard is built by shardFor and selfPinned starts false even though the row
	// it restored was written by that agent's own session_start. Code asking
	// "did this agent choose its root?" must accept either — see the
	// declaration-refusal marker in repinAgent.
	restored bool
	// parentSeeded: seeded from its conversation's PERSISTED pin (parent not in
	// memory), so a connection move must not drag it (#513).
	parentSeeded bool

	// rosterID is the session.Info registered for THIS agent: the row the
	// workspace it works in lists it by (issue #472) and, for every agent but the
	// connection's owner, the identity peers address and its commits are signed
	// with (#556). Empty until the agent first needs one; guarded by mu with the
	// scalars above. rosterFolder is the Folder last written to the row, so a root
	// that moved since is noticed without reading the row back.
	rosterID     string
	rosterName   string
	rosterFolder string
	// regMu serialises registering and retiring the row, which is disk I/O under
	// the session-directory flock and so must never run under mu: held across the
	// I/O, taking mu only for the brief reads and writes inside it (order: regMu,
	// then mu).
	regMu sync.Mutex

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
	//
	// restoresShardFor is the other way in: after a restart the identities seen
	// start empty, so the first stamped caller reads as the only agent, yet durable
	// evidence says the connection was shared (#523). Any stamped agent is routed
	// to its shard then, whether or not it holds a pin row: a subagent anchored to
	// its parent's root has none. Routing only; nothing that gates a call consults it.
	if !s.logicalAgents.sharedWith(id) && !s.restoresShardFor(id) {
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
	return s.shardOf(id)
}

// shardOf returns the shard for id, creating it on first use seeded as shardFor's
// doc describes. It is the creation half of shardFor, split out for the one caller
// that needs a shard the routing rule did not ask for: the identity of a subagent
// that is alone on its connection (conn_agent_identity.go callerFor).
func (s *connSession) shardOf(id string) *agentShard {
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
		prov:         pinProvenanceOf(&v),
	}
	// A hook-stamped subagent starts where its CONVERSATION chose to work, not
	// where the connection happens to sit (issue #513 review). Seeding it from
	// the connection pin sent a subagent of a parent that had re-pinned itself
	// to a worktree into whichever checkout the connection held — another
	// agent's — the exact misroute the declaration gate exists to prevent.
	s.seedFromParentLocked(sh)
	// Restore a pin this agent persisted before the restart (PLAN-286): it takes
	// precedence over the connection's current pin. A pin that no longer verifies
	// is ignored, so the shard keeps the connection's root rather than resurrecting
	// a deleted or widened one.
	if root, language, origin, ok := s.loadPinForAgent(id); ok {
		if resolved, _, intact := s.restoreRootIntact(root); intact {
			sh.root = resolved
			sh.language = language
			sh.pinOrigin = origin
			sh.prov = restoredProvenance(origin)
			// A persisted per-agent row is a root this agent DECLARED (only
			// repinAgent's move path, confirmShardPin and attributeConnectionPin
			// write one), so the declaration-refusal marker must not treat it as a
			// seed, and a connection move must not drag it (followsConnectionLocked).
			sh.restored = true
		}
	}
	sh.policy = s.buildAgentPolicy(sh.root, sh.language, sh.prov)
	// The agent that WAS the connection until a peer turned it shared has its
	// strict-mode reads in the connection tracker, persisted under the empty
	// agent id where the per-agent rehydration below cannot see them. Seed its
	// shard from that tracker, so an edit it has in flight does not fail "has
	// not been read" the moment a subagent appears. Every other agent starts
	// empty. See seedsConnectionReads for who qualifies.
	if s.seedsConnectionReads(id, sh.root, v.acquiredRoot) {
		sh.readTracker.Hydrate(s.readTracker.Records())
	}
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
// dep-roots). The sessionView is copied and its root/language overridden, so
// buildPathPolicy's many config reads stay unchanged while the per-agent facts
// come from the shard — including the pin provenance a boundary refusal quotes,
// which is the shard's own pin's and not the connection's (#529).
func (s *connSession) buildAgentPolicy(root, language string, prov tools.PinProvenance) *tools.PathPolicy {
	v := s.view()
	v.acquiredRoot = root
	v.acquiredLanguage = language
	v.pinVia, v.pinAt, v.pinPrev, v.pinForced = prov.Source, prov.At, prov.Previous, prov.Forced
	return s.buildPathPolicy(&v)
}

// workspaceFor returns the workspace pinned for the logical agent in ctx, falling
// back to the connection's pin when the connection is not shared (or the call is
// unattributed). workspace() stays the ctx-less default for background goroutines.
func (s *connSession) workspaceFor(ctx context.Context) string {
	if _, _, pending := s.pendingDeclarationForCall(ctx); pending {
		// A refused declaration leaves nothing trustworthy to anchor to: the
		// shard's root is one this agent explicitly tried to leave. "" makes the
		// implicit resolvers — relative paths, git's default repository,
		// orientation — refuse rather than resolve inside it, and the boundary
		// guard's own error names the remedy. Without this the agent keeps
		// working in the seeded root and only a human notices.
		return ""
	}
	if sh := s.shardFor(ctx); sh != nil {
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		return sh.root
	}
	return s.workspace()
}

// recordedRootFor is the workspace a COMPLETED call by agentID ran against:
// the root of that agent's shard, or "" when the agent has none (an
// unattributed call, or a connection only one agent holds).
//
// Unlike workspaceFor it never creates a shard. It runs on the after-tool
// recording path, after the work is done, where the only honest answer is the
// root the call actually used — and where creating a shard would do the
// sessionstate pin lookup shardFor does, on the response path, for an agent
// whose call never needed one.
func (s *connSession) recordedRootFor(agentID string) string {
	if agentID == "" {
		return ""
	}
	s.shardsMu.Lock()
	sh := s.shards[agentID]
	s.shardsMu.Unlock()
	if sh == nil {
		return ""
	}
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.root
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
//
// prev is the shard's root BEFORE the call, read under the same sh.mu
// acquisition that moves it, so session_start can report the pin's previous
// root without a second, racy read (issue #517).
func (s *connSession) repinAgent(ctx context.Context, root, language string, origin sessionstate.PinSource, force bool) (prev string, changed bool, refused error) {
	sh := s.repinShard(ctx)
	if sh == nil {
		return "", false, nil
	}
	// The roster sync registers or moves a session.Info, which takes a flock on
	// the session directory. Doing that while holding sh.mu wedges the whole
	// connection: a peer's ordinary call reaches boundaryPolicy, which walks the
	// shards under shardsMu and blocks on THIS shard's RLock, and every other
	// agent then blocks on shardsMu behind it — all waiting on one agent's disk
	// I/O. Registered before the unlock defer so LIFO runs it AFTER sh.mu is
	// released, and it re-takes the lock itself.
	var syncRoot, syncLang, movedFrom string
	defer func() {
		if syncRoot != "" {
			s.syncAgentRoster(sh, syncRoot, syncLang)
		}
		// After sh.mu is released: it takes shardsMu, then each shard's mu.
		if movedFrom != "" {
			s.followParentShard(sh.id, movedFrom)
		}
	}()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	prev = sh.root
	// The guard keys on the pin ORIGIN, which a seeded shard inherits wholesale:
	// shardFor copies the CONNECTION's pin and its origin onto a new shard, and
	// attachOrRepinTo's same-root promotion branch upgrades a roots-held pin to
	// PinSourceSessionStart whenever any caller names the current root — silently,
	// since no root moves. Every shard built afterwards therefore carried a
	// session_start origin nobody had set on its behalf, and the guard refused
	// that agent's FIRST explicit pin as a drift away from a workspace it had
	// never held, offering force: true — which displaces a peer on exactly the
	// pooled connection where this arises — as the only remedy (issue #468).
	//
	// correctsSeededRoot, not !selfPinned alone, is the exemption: an agent that
	// chose nothing may correct its root only WITHIN the tree it was seeded in.
	// A move to an UNRELATED workspace stays refused however the shard got its
	// root — that is the fail-closed #182/PLAN-395 guarantee, and relaxing it
	// would be worse than the drift being fixed here. PLAN-398 closed the half of
	// this where the connection moved afterwards; this closes the half where it
	// did not.
	if !force && prev != "" && root != prev &&
		!correctsSeededRoot(sh.selfPinned, prev, root) &&
		sh.pinOrigin == sessionstate.PinSourceSessionStart {
		// Classified with the scope the caller needs to choose a recovery: at
		// "agent" scope force: true moves only THIS agent's shard, so an automatic
		// retry is safe here (and is not at connection scope, conn_repin.go).
		// Error() stays byte-identical — the classification is a side-car.
		refused = toolerror.Wrap(
			fmt.Errorf("refusing to re-pin logical agent %q from %s to %s: this agent's pin was set by an explicit session_start and is sticky — issue #182. To switch this agent's project, call session_start again with force: true. For agents sharing a connection: %s", mcp.LogicalAgentFromCtx(ctx), prev, root, tools.PerCallIdentityRemedy),
			toolerror.KindPinRefused,
			toolerror.ClassPassForce,
			toolerror.WithTool("session_start"),
			toolerror.WithDetail("scope", "agent"),
			toolerror.WithDetail("agent", sh.id),
			toolerror.WithDetail("pinned", prev),
			toolerror.WithDetail("requested", root),
		)
		// Leave a trace on this past-vulnerability surface: the connection-level
		// guard has always logged a refused steal, and a refused cross-workspace
		// drift on a SHARED connection is exactly the event an operator needs to
		// find afterwards. The daemon log is the whole trace, deliberately — see
		// the note on repinAgent.
		// Not repinStickyRemedy: that one opens by telling the caller to identify
		// itself, which an agent holding its own shard has already done. The
		// refusal's own text carries the remedy that applies here.
		// Only a shard that never CHOSE a root can reach here with an unrelated
		// target (a self-pinned shard is exempt from correctsSeededRoot above),
		// which is exactly the case where the root it keeps is someone else's.
		// Record it so the agent's next path-bearing call is refused with the
		// remedy instead of being resolved into that root.
		if !sh.selfPinned && !sh.restored {
			s.markDeclarationRefused(mcp.LogicalAgentFromCtx(ctx), root, prev)
		}
		s.log().Warn("daemon: per-agent session_start re-pin refused — this agent's pin is sticky (issue #182)",
			"agent", sh.id, "pinned", prev, "requested", root,
			"remedy", "call session_start again with force: true to move THIS agent, or run one plumb serve per agent")
		return prev, false, refused
	}
	if root == prev && language == sh.language {
		// Nothing moves — but naming the root the shard already holds is still
		// a CHOICE, which is what the comment below has always claimed
		// ("even back to the seeded one"). Returning before selfPinned was set
		// meant an agent that confirmed its seeded workspace kept FOLLOWING the
		// connection, so a later connection move — a roots notification, or an
		// anonymous forced re-pin — dragged it off a workspace it had
		// explicitly named, with no call of its own in between (issue #468).
		s.confirmShardPin(sh, root, language, origin)
		syncRoot, syncLang = root, language
		// Also on the confirm branch, not just on a move. A shard restored after
		// a daemon restart already holds a root that may differ from the
		// connection's, and the reconnecting agent's next session_start NAMES
		// that root — which lands here, not in the move branch below. Syncing
		// only on a move therefore left a restored agent exactly as invisible as
		// issue #472 describes, by a path no live-move test exercises.
		// confirmShardPin cannot host this: it returns early for a shard that is
		// already selfPinned, which a restored-and-reconfirming one is.
		return prev, false, nil
	}
	changed = true
	// The agent has CHOSEN this root (even back to the seeded one, via a
	// deliberate re-pin): from here the shard no longer follows the connection
	// (PLAN-398).
	sh.selfPinned = true
	sh.root = root
	sh.language = language
	sh.pinOrigin = origin
	// Not Forced, though force may have been passed: what a force overrides here is
	// this agent's OWN earlier pin, and Forced is the claim that someone else's
	// was displaced — it makes a refusal tell the caller another agent took its
	// pin, which an agent that moved itself did not suffer.
	sh.prov = tools.PinProvenance{Source: pinViaLabel(origin, pinTriggerLive), At: time.Now(), Previous: prev}
	sh.policy = s.buildAgentPolicy(root, language, sh.prov)
	// Read/write/undo state is workspace-relative, so only a MOVE invalidates
	// it. A same-root language switch changes no file — and it is reachable
	// without an override at all, since repinWorkspaceFrom passes Detect's
	// language, so a bare re-orienting session_start after `plumb enable-lsp`
	// used to wipe an agent's dirty-guard writes and undo history (PLAN-428).
	// The connection path keeps them under the same rule.
	if root != prev {
		movedFrom = prev
		sh.readTracker.Reset()
		sh.writeTracker.Reset()
		sh.undoStore.Reset()
	}
	s.rehydrateReadsForAgent(sh, root)
	s.persistPinForAgent(sh, root, language, origin)
	syncRoot, syncLang = root, language
	return prev, changed, nil
}

// seedShardOnLink hydrates the linkage owner's shard from the connection's
// read tracker when the linkage was established AFTER that shard was created
// — the one ordering session_start can produce, since its Execute re-pins
// (creating the shard) before it resolves linkage. Without this the parent's
// first session_start, if it also names a workspace, caches a shard that
// seedsConnectionReads judged against an empty external id and never revisits.
//
// Only an EMPTY tracker is filled, and only for a shard sitting on the
// connection's own root, so this can neither overwrite reads the agent has
// since made nor resurrect reads for a project the shard is not pinned to.
// Lock order is the documented one: shardsMu before sh.mu.
func (s *connSession) seedShardOnLink(linkage string) {
	if linkage == "" {
		return
	}
	connRoot := s.view().acquiredRoot
	s.shardsMu.Lock()
	defer s.shardsMu.Unlock()
	sh, ok := s.shards[linkage]
	if !ok {
		return
	}
	sh.mu.RLock()
	root := sh.root
	sh.mu.RUnlock()
	if root != connRoot || len(sh.readTracker.Records()) > 0 {
		return
	}
	sh.readTracker.Hydrate(s.readTracker.Records())
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
