package cli

// conn.go — per-connection MCP session state and behaviour.
//
// connSession holds the mutable state shared across all closures that serve
// one MCP connection. Methods on connSession host the bodies of what were
// previously anonymous closures inside handleConn, keeping handleConn itself
// a thin orchestrator (see daemon.go).
//
// The session behaviour is split across files by concern: workspace attach /
// re-pin / language detection live in conn_attach.go; per-project config
// apply/watch and the shared write-budget binding in conn_config.go; the
// topology + quality subsystems, Java post-write notify, and stats recording in
// conn_subsystems.go; write-deps assembly and MCP tool/hook registration in
// conn_register.go; the session's own name, addressability and purpose tag in
// conn_identity.go. This file holds the session state, the copy-on-write
// mutation lane, and the lock-free accessors.

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/quality"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// sessionView is an immutable snapshot of a connSession's mutable state.
// Readers load it lock-free via connSession.state (an atomic.Pointer); mutators
// copy-on-write under muMutate (see mutate). A loaded *sessionView is treated as
// read-only — never mutate one in place.
type sessionView struct {
	acquiredRoot, acquiredLanguage string
	// lsRefRoot is the workspace root for which this session holds a PINNED
	// reference on the shared language-server pool entry (set on a successful
	// attach / re-pin, "" when the session attached without LSP). Released on
	// re-pin (old root) and on close so the pool can reclaim an idle server once
	// its last session leaves. Distinct from acquiredRoot, which is also set for
	// LanguageNone workspaces that hold no LS reference.
	lsRefRoot string
	// lsRefLang is the language of the pinned pool entry referenced by lsRefRoot.
	// Paired with lsRefRoot so the release on close / re-pin targets the exact
	// (root, language) entry this session pinned, now that one root may host
	// several language servers.
	lsRefLang                 string
	clientName, clientVersion string
	// protocolOffered/protocolAnswered and clientCaps are the initialize-time
	// MCP protocol negotiation snapshot (see onProtocolNegotiated).
	protocolOffered, protocolAnswered string
	clientCaps                        json.RawMessage
	sessName                          string
	// purpose is the optional human-readable session tag set via session_start's
	// purpose arg. Descriptive only; stamped on this session's stats rows and
	// surfaced by daemon_info. "" when unset.
	purpose      string
	lastCfgMtime time.Time
	// projectWatchRoot: the canonical root this session holds a project-config
	// watcher reference on (PLAN-414), acquired on every config apply, released
	// on re-pin / close. fallbackWarned latches the one-time poll-fallback log
	// line. collabNotice is a pending one-shot [collab] capability-change
	// notice, surfaced on the next tool result via enrichToolOutput.
	projectWatchRoot string
	fallbackWarned   bool
	collabNotice     string
	// boundBudgetKey is the (client, workspace) key of the shared write budget
	// this session currently holds a reference on (see sharedBudgets), or "" when
	// none is held. Released and re-acquired on re-pin, released on close, so a
	// budget entry is reclaimed once its last session leaves.
	boundBudgetKey string
	// notify sends a server-initiated notification to this connection's client.
	// Captured at OnInit; nil-safe (it is nil in tests and before initialize).
	notify mcp.NotifyFn
	// lastToolProfile is the resolved tool profile ("lean"/"full") last advertised
	// to the client, seeded at OnInit. A config reload that changes the resolved
	// profile fires a notifications/tools/list_changed against this seed, so the
	// first real change is detected and no spurious notification fires at startup.
	lastToolProfile string

	edits     config.EditsConfig
	walk      config.WalkConfig
	git       config.GitConfig
	ws        config.WorkspaceConfig
	semantics config.SemanticsConfig
	memory    config.MemoryConfig
	collab    config.CollabConfig
	tools     config.ToolsConfig
	session   config.SessionConfig
	// tasks holds the resolved [tasks.<lang>] command templates; agentConfigWrites
	// is the resolved enable knob for the agent-writable-config tool. Both are
	// swapped per project on every attach / re-pin / reload, like the blocks above.
	tasks             map[string]config.TasksConfig
	agentConfigWrites bool
	// commands is the resolved [[command]] allow-list; commandPolicy is the
	// resolved [commands] table (require_sandbox). Both are swapped
	// per project on every attach / re-pin / reload, like the blocks above; the
	// trust gate is applied at the resolver seam (conn_commands.go).
	commands      []config.CommandConfig
	commandPolicy config.CommandsConfig
	// execTrusted is whether this workspace may run the commands its project
	// config supplies (run_command's [[command]] entries, the xcode build
	// server). Resolved at config apply from the SAME bytes as
	// commands/commandPolicy above, so the authorisation and the thing it
	// authorises can never disagree — see config.ExecTrustedFor. Re-checking at the
	// point of use would let a repository load hostile content and then restore the
	// file to content that is trusted.
	execTrusted bool
	// projectCommands: does this project supply [[command]] at all, in ANY
	// spelling? Resolved from the policy spec at apply, never re-read — see
	// conn_commands.go for the case-fold bypass a re-read caused.
	projectCommands bool
	// projectGit is what this project's config asks for in the capability-granting
	// sections and whether it is trusted, captured at apply beside the git block it
	// describes so session_start's notice and the policy it annotates are one
	// snapshot. Never re-read — see conn_config.go.
	projectGit tools.ProjectGitStatus

	// Live subsystem handles are pointers — cheap to copy into the snapshot and
	// swapped (never mutated) on attach / re-pin / reconcile.
	qualityRunner *quality.Runner
	topologyStore *topology.Store
	policy        *tools.PathPolicy // built eagerly on the mutation path; see boundary_policy.go

	// depRoots holds the session language's read-only toolchain dependency roots
	// (e.g. Go's GOMODCACHE/GOROOT, Zig's stdlib + cache, Python's stdlib +
	// site-packages). They are toolchain-global (workspace-independent), computed
	// off the mutation lane by warmDepRoots and folded into policy once known.
	// depRootsLang records the language they were resolved for, so buildPathPolicy
	// only admits them while the session stays on that language — a cross-language
	// re-pin re-warms before the new language's roots become readable.
	depRoots     []tools.AllowedRoot
	depRootsLang string

	// discoveredLangs is the distinct set of child languages found at attach for a
	// monorepo root (the elected primary plus its lazily-attached siblings), or nil
	// for a single-language root. Surfaced as the multi-language session_start
	// identity line (e.g. "Swift, Zig").
	discoveredLangs []string

	// allowDirs are extra read-write roots the client granted at connection time
	// via `plumb serve --allow-dir` / PLUMB_ALLOWED_DIRS, transported in the
	// initialize params' _meta (see onAllowDirs). They are per-connection — never
	// shared with another session — and folded into the PathPolicy by
	// buildPathPolicy as read-write roots, additive to the workspace and config
	// extra_roots. On an unattached serve (no --workspace/PLUMB_WORKSPACE and no
	// session_start yet) the grant is inert: buildPathPolicy returns nil while no
	// workspace is pinned, so the boundary keeps failing closed, and the roots
	// attach additively to whatever workspace session_start later pins — a grant
	// is never the source of a workspace. Set once during the initialize
	// exchange, before attach, and preserved across re-pins (a re-pin keeps the
	// client's grant).
	allowDirs []string

	// proxySessionID is the stable per-proxy session ID `plumb serve` transported
	// in the initialize params' _meta (see onProxySession). It keys the persisted
	// per-connection state (read-tracking, pinned workspace) so a reconnected
	// connection after a daemon restart rehydrates rather than starting fresh. Set
	// once during the initialize exchange, before attach, and preserved across
	// re-pins. "" when the client is not a session-id-injecting serve proxy.
	proxySessionID string
	// replayedSessionID is the stable plumb session ID the serve proxy replayed
	// in _meta[MetaSessionIDKey] (PLAN-296). Observability today; adoption makes
	// the live sessID equal to it on reconnect.
	replayedSessionID string
	// persistedIdentity is the durable identity record selected by this
	// connection's proxy session ID (captured by restoreIdentity). It is the sole
	// authorisation for resuming a predecessor's ID, name and mailbox: the proxy
	// session ID is a never-disclosed bearer secret — presenting it is evidence of
	// being the same serve process — whereas a plumb session ID or name is echoed
	// to clients, so a replayed one is only a CLAIM. The record is what turns the
	// claim into proof; a zero value is "no proof", never a wildcard.
	persistedIdentity sessionstate.Identity
	// recovery is what happened to this connection's identity during initialize
	// (see recoveryOutcome). It is reported to the proxy in the initialize result
	// _meta, which is what lets a reconnect note state an outcome rather than
	// assume one. "" until restoreIdentity runs, and read through recovery().
	recovery recoveryOutcome
	// inheritedSessionIDs are predecessor plumb session IDs this connection may
	// also read mailbox messages for, granted ONLY by the proxy-authenticated
	// persisted-state path (see inheritSessionID). Nil for every other session.
	inheritedSessionIDs []string

	// workspaceHint is the workspace pre-pin the serve proxy transported in the
	// initialize params' _meta — the explicit --workspace/PLUMB_WORKSPACE value
	// (see onWorkspaceHint); the serve's working directory is never transported,
	// so a serve started without one leaves this empty and the connection
	// unattached until session_start pins it. Consulted only as the last attach
	// fallback before tool-path seeding, always validated through pool.Detect,
	// and never persisted as the sticky pin — so it can inform an attach but
	// never overwrite a workspace the caller deliberately chose. Set once during
	// the initialize exchange and preserved across re-pins. "" when the client
	// sent no workspace pre-pin.
	workspaceHint string

	// replayedPin is the workspace the caller chose with an explicit session_start,
	// replayed by the serve proxy in _meta[mcp.MetaPinnedWorkspaceKey] after a
	// daemon restart. Authoritative — it outranks a client-reported root. Empty on
	// a first connect and on a proxy that predates the key.
	replayedPin string

	// pinVia / pinAt / pinPrev are pin-drift observability for issue #182: the
	// label of the source that last set this connection's pin (see pinViaLabel),
	// when it was set, and the root it replaced. pinOrigin is the structured
	// origin behind the label — the sticky-pin guard keys on it, not the label.
	// Stamped inside the attach / re-pin mutate beside acquiredRoot, so they can
	// never disagree with the pin they describe. Zero while unattached.
	pinVia, pinPrev string
	pinAt           time.Time
	pinOrigin       sessionstate.PinSource
	// pinForced records that this pin overrode the sticky-pin guard with
	// force: true — it did not merely change the workspace, it DISPLACED a pin
	// another caller on this connection had deliberately set. Stamped in the same
	// mutate as pinPrev, which names what was displaced; the two are only
	// meaningful together. Surfaced to the displaced caller through
	// tools.PinProvenance (see DisplacementNotice), because that caller is
	// otherwise the only party to the event with no signal at all.
	pinForced bool

	// pinUnverifiedReplay marks a pin that arrived over the serve proxy's
	// initialize _meta channel, which the daemon cannot authenticate (issue
	// #318). Such a pin records PinSourceSessionStart — deliberately, so rank,
	// stickiness and what gets persisted are unchanged — but that origin is ALSO
	// what the live containment re-check keys on, and the re-check is the half of
	// #306's guard that exists for the swap attack a client mounting this channel
	// is best placed to run: it names the root, so it can replace that directory
	// with a symlink to a home container after attach. This flag is how
	// policyRootRefused tells the two apart without demoting the origin.
	//
	// Cleared by recordPinProvenance, which every pin write runs through, so it
	// describes the CURRENT pin and a later deliberate session_start clears it.
	pinUnverifiedReplay bool
}

// connSession holds all per-connection state for an MCP session. The mutable,
// copyable part lives in an immutable sessionView loaded lock-free via state
// (atomic.Pointer); every mutation goes through mutate, which serialises on
// muMutate, shallow-copies the current view, applies the change, and atomically
// swaps in the new pointer. Readers therefore never block and never observe a
// torn view; mutations (attach, re-pin, config reload, rename) are rare and run
// one at a time through the single lane. requestMu (the client-request callback)
// and watcherOnce are orthogonal and kept as-is. All exported methods are safe
// for concurrent use.
type connSession struct {
	pool       *workspacePool
	store      *config.Store
	statsStore *statsStore
	// sessionState persists read-tracking + the pinned workspace across daemon
	// restarts. nil when persistence could not be opened ⇒ all persist/rehydrate
	// calls no-op (see conn_persist.go).
	sessionState *sessionstate.Store
	budgets      *sharedBudgets
	// daemonStartedAt is this daemon process's start time. It is the txlog
	// orphan-recovery cutoff: Scan only rolls back tx-log dirs older than it, so a
	// connection's attach never reverts a live transaction another connection
	// started this run.
	daemonStartedAt time.Time

	// sessID is the plumb session ID this connection is registered under. It is
	// mutable: onSessionID may ADOPT the stable ID the serve proxy replayed
	// (PLAN-296), which re-keys the session file and the connRegistry. Every
	// read goes through sessionID(); the only writer is setSessionID.
	sessIDMu sync.RWMutex
	sessID   string

	// registry is the daemon's live-connection registry, keyed by session ID.
	// onSessionID re-keys it when the stable replayed ID is adopted (PLAN-296).
	// nil in tests that construct connSession directly rather than via handleConn.
	registry *connRegistry

	// projectWatches is the daemon-owned per-workspace project-config watcher
	// manager (PLAN-414); nil in tests — then the 30s poll is the only reload
	// mechanism, as before.
	projectWatches *projectConfigWatchManager

	// logicalAgents observes the distinct logical-agent identities this
	// connection declares (session_start.session_id and per-call _meta), so a
	// multiplexing client can be detected before it shares state (PLAN-286).
	logicalAgents logicalAgentState

	// pinContest is the connection's recent forced-pin-displacement history. It
	// is how a multiplexing client that declares NO identity is detected —
	// logicalAgents cannot see one, so the only evidence is the behaviour: a pin
	// force-taken between projects, repeatedly. See conn_pin_contest.go.
	pinContest pinContestState

	// shards holds the per-logical-agent copies of the mutable facts, keyed by
	// logical-agent ID. Populated only once the connection is shared; guarded by
	// shardsMu. See conn_agent_shard.go.
	shardsMu sync.Mutex
	shards   map[string]*agentShard

	ctx    context.Context
	cancel context.CancelFunc

	state    atomic.Pointer[sessionView] // lock-free reads of the session snapshot
	muMutate sync.Mutex                  // the single mutation lane (see mutate)

	// lspGenSeen is the pool language-set generation this connection last resolved
	// its primary language at (see workspacePool.langsGen). Deliberately NOT a
	// sessionView field: every tool call compares it, so it must stay a lock-free
	// atomic outside the copy-on-write snapshot.
	lspGenSeen atomic.Uint64

	sessionProxy *routingProxy
	sessionInv   *routingInvProxy
	sessionCache *cache.Cache
	readTracker  *tools.ReadTracker
	writeTracker *tools.WriteTracker
	undoStore    *tools.UndoStore
	ttl          time.Duration

	topologyPool *topologyPool
	memoryPool   *memoryIndexPool
	collabPool   *collabPool
	hintCache    *memoryHintCache
	peerWrites   *peerWriteCache
	chatWatch    *chatWatch
	writeLimiter *tools.RateLimiter

	// hintSeen tracks the memory names already hinted on this connection, so a
	// memory is pointed out once per session, not on every read of a hot path.
	// Lazily created; cleared on re-pin.
	hintSeen   map[string]bool
	hintSeenMu sync.Mutex

	watcherOnce    sync.Once
	xcodeStartedMu sync.Mutex
	xcodeStarted   map[string]bool // roots already evaluated for next-session Xcode config
	unsubscribe    func()          // removes the store-change listener on close

	clientRequest mcp.RequestFn
	requestMu     sync.RWMutex

	// logger carries the session_id attribute so per-connection log records can
	// be correlated across the interleaved daemon.log output. Global daemon-level
	// log calls (pool lifecycle, config watcher, start/stop) keep using the
	// package-level slog functions and are intentionally not tagged.
	logger *slog.Logger
}

// view returns the current session snapshot, or a zero sessionView when none has
// been installed yet (struct-literal construction in tests). Never returns nil.
func (s *connSession) view() sessionView {
	if v := s.state.Load(); v != nil {
		return *v
	}
	return sessionView{}
}

// mutate serialises a copy-on-write update of the session snapshot: it copies the
// current view, applies fn, and atomically stores the result. fn MUST NOT call
// mutate again (re-entrant deadlock) — compose all field writes for one logical
// change into a single fn. Slow work fn performs (LSP acquire, session.Patch,
// quality teardown) runs under muMutate exactly as the prior stateMu did, but
// readers are lock-free and never block on it.
func (s *connSession) mutate(fn func(v *sessionView)) {
	s.muMutate.Lock()
	defer s.muMutate.Unlock()
	cur := s.view()
	fn(&cur)
	s.state.Store(&cur)
}

// newConnSession initialises a connSession and registers a new MCP session.
// Call close() when the connection ends.
// The session context is derived from parent (the daemon context) so a
// daemon-wide shutdown cancels every session; s.cancel() additionally lets the
// idle reaper cancel one session in isolation. handleConn drives mcp.Serve on
// s.ctx, so either cancellation makes Serve return and the deferred cleanup run.
func newConnSession(parent context.Context, pool *workspacePool, topoPool *topologyPool, store *config.Store, statsStore *statsStore, sessState *sessionstate.Store, budgets *sharedBudgets) *connSession {
	cfg := store.Current()
	ttl := cfg.Cache.TTL.Duration
	// Register assigns the name, under the session-directory flock, so it cannot
	// land on one a live session already answers to — names are mailbox addresses
	// and a duplicate silently misdelivers.
	//
	// On a registration failure the session still runs, unregistered, as before.
	// The fallback name is a DISPLAY name only: it was drawn without a uniqueness
	// check and the session has no file for anyone else's check to find, so it
	// must not become an address. addressableName gates that on sessID, which
	// stays empty here.
	//
	// The draw also avoids names RESERVED by identities that are recoverable but
	// not currently live — a `plumb serve` outliving its daemon has no live record
	// at all, and handing its name to this new session would orphan every note
	// addressed to it once it reconnects and is renamed by the collision path. A
	// store that cannot answer contributes nothing, leaving the live-session check
	// exactly as it was.
	reg, err := session.RegisterReserved(session.Info{DaemonVersion: Version}, reservedNamesFrom(sessState))
	if err != nil {
		slog.Warn("daemon: session registration failed; continuing unregistered and unaddressable", "err", err)
		reg.Name = session.GenerateName()
	}
	sessName, sessID := reg.Name, reg.ID
	ctx, cancel := context.WithCancel(parent)
	s := &connSession{
		ctx:          ctx,
		cancel:       cancel,
		pool:         pool,
		topologyPool: topoPool,
		hintCache:    &memoryHintCache{},
		peerWrites:   &peerWriteCache{},
		chatWatch:    &chatWatch{},
		store:        store,
		statsStore:   statsStore,
		sessionState: sessState,
		budgets:      budgets,
		sessID:       sessID,
		ttl:          ttl,
		sessionProxy: newRoutingProxy(pool),
		sessionInv:   newRoutingInvProxy(pool),
		sessionCache: cache.New(ttl),
		readTracker:  tools.NewReadTracker(),
		writeTracker: tools.NewWriteTracker(),
		undoStore:    tools.NewUndoStore(),
		writeLimiter: tools.NewRateLimiter(cfg.Edits.RateLimitPerMinute, time.Minute),
		logger:       slog.Default().With("session_id", sessID),
	}
	// Seed the language-set generation so only a widening AFTER this connection
	// was built triggers a primary refresh; a connection that attaches later
	// resolves against the current set anyway.
	if pool != nil {
		s.lspGenSeen.Store(pool.langsGeneration())
	}
	s.state.Store(&sessionView{
		sessName:  sessName,
		edits:     cfg.Edits,
		walk:      cfg.Walk,
		git:       cfg.Git,
		ws:        cfg.Workspace,
		semantics: cfg.Semantics,
		memory:    cfg.Memory,
		collab:    cfg.Collab,
		tools:     cfg.Tools,
		session:   cfg.Session,
	})
	// Mirror every strict-mode read to the durable store so it survives a daemon
	// restart; the sink reads the live view (proxy id + workspace + gate) per call,
	// so it is correct across re-pins and a no-op when persistence is off.
	s.readTracker.SetPersistSink(s.persistRead)
	// Re-merge the per-project view whenever the global base config changes, so
	// a global edit (TUI, external editor, or `plumb config reload`) propagates
	// to every live session without a daemon restart.
	s.unsubscribe = store.Subscribe(func(config.Config) {
		if ws := s.workspace(); ws != "" {
			s.applyProjectConfig(ws)
			s.reconcileTopologyStore(ws)
			s.log().Info("daemon: global config changed — session re-applied", "workspace", ws)
		}
	})
	return s
}

// close releases per-session resources and unregisters the session.
func (s *connSession) close() {
	if s.unsubscribe != nil {
		s.unsubscribe()
	}
	// An intent must not outlive its session — clear this session's intents while
	// the workspace is still known (before cancel/teardown). Notes survive.
	s.clearSessionIntents()
	s.cancel()
	s.sessionCache.Close()
	// Stop the quality runner, release this session's pinned language-server
	// reference so the pool can reclaim the server once its last session leaves
	// (after the idle grace), and drop its shared write-budget reference so that
	// entry is reclaimed too — all under the one mutation lane.
	var ref, refLang, budgetKey string
	s.mutate(func(v *sessionView) {
		if v.qualityRunner != nil {
			v.qualityRunner.Stop()
			v.qualityRunner = nil
		}
		ref = v.lsRefRoot
		refLang = v.lsRefLang
		v.lsRefRoot = ""
		v.lsRefLang = ""
		budgetKey = v.boundBudgetKey
		v.boundBudgetKey = ""
	})
	s.releaseProjectWatch()
	if ref != "" {
		s.pool.release(ref, refLang)
	}
	if budgetKey != "" {
		s.budgets.release(budgetKey)
	}
	session.Unregister(s.sessionID())
}

// log returns the session-scoped logger, falling back to the process-global
// default logger when the field has not been initialised (e.g. in tests that
// construct connSession directly rather than via newConnSession).
func (s *connSession) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// workspace returns the resolved workspace root for the session.
func (s *connSession) workspace() string {
	return s.view().acquiredRoot
}

// markBoundaryViolation records the violation on the session record and is
// deliberately sticky-not-terminating: each offending tool call already gets a
// WorkspaceBoundaryError back, which is the per-call enforcement contract.
// "Health: blocked" + HealthMessage is observability — the TUI dashboard alert
// and the session detail pane both render this message directly (issue #358),
// so every caller of this method must name its own remedy rather than leaving
// the reader with no next step, while legitimate calls inside the pinned
// workspace keep working. We do not
// cancel s.ctx here: a single confused tool call (e.g. an agent fumbling a
// path) should not tear down an otherwise-working session, and the boundary
// error is informative enough for the caller to course-correct.
func (s *connSession) markBoundaryViolation(message string) {
	if message == "" {
		return
	}
	session.Patch(s.sessionID(), func(info *session.Info) {
		info.Health = "blocked"
		info.HealthMessage = message
	})
}

// clientNameStr returns the MCP client name for the session.
func (s *connSession) clientNameStr() string {
	return s.view().clientName
}

// setClientRequest stores the latest MCP RequestFn for subsequent rootsFn calls.
func (s *connSession) setClientRequest(req mcp.RequestFn) {
	s.requestMu.Lock()
	s.clientRequest = req
	s.requestMu.Unlock()
}
