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
	// resolved [commands] table (allow_shell / require_sandbox). Both are swapped
	// per project on every attach / re-pin / reload, like the blocks above; the
	// trust gate is applied at the resolver seam (conn_commands.go).
	commands      []config.CommandConfig
	commandPolicy config.CommandsConfig
	// execTrusted is whether this workspace may run the commands its project
	// config supplies (run_command's [[command]] entries, execute_shell_command,
	// the xcode build server). Resolved at config apply from the SAME bytes as
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
	// extra_roots. Set once during the initialize exchange, before attach, and
	// preserved across re-pins (a re-pin keeps the client's grant).
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
	// inheritedSessionIDs are predecessor plumb session IDs this connection may
	// also read mailbox messages for, granted ONLY by the proxy-authenticated
	// persisted-state path (see inheritSessionID). Nil for every other session.
	inheritedSessionIDs []string

	// workspaceHint is the serve proxy's advisory working directory, transported
	// in the initialize params' _meta (see onWorkspaceHint). Consulted only as
	// the last attach fallback before tool-path seeding, always validated through
	// pool.Detect, and never persisted as the sticky pin — so it can inform an
	// attach but never overwrite a workspace the caller deliberately chose. Set
	// once during the initialize exchange and preserved across re-pins. "" when
	// the client is not a cwd-injecting serve proxy.
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

	// logicalAgents observes the distinct logical-agent identities this
	// connection declares (session_start.session_id and per-call _meta), so a
	// multiplexing client can be detected before it shares state (PLAN-286).
	logicalAgents logicalAgentState

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
	reg, err := session.Register(session.Info{DaemonVersion: Version})
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

// acquiredLanguageName returns the LSP language attached to this session, or ""
// when none is (LanguageNone, or not yet attached). session_start uses it to
// distinguish a real "LSP is ready" from a marker-detected project whose
// server is opt-in/off/missing — it must not advertise LSP tools that error.
func (s *connSession) acquiredLanguageName() string {
	lang := s.view().acquiredLanguage
	if lang == "" || lang == LanguageNone {
		return ""
	}
	return lang
}

// acquiredLanguageLabels returns the distinct child languages discovered for a
// monorepo root (the elected primary plus its siblings), as [lsp.<lang>] keys,
// or nil for a single-language root. session_start renders these as the
// "Language: Swift, Zig" identity line; the single primary still drives the
// recommended-step guidance via acquiredLanguageName.
func (s *connSession) acquiredLanguageLabels() []string {
	return s.view().discoveredLangs
}

// lspWarming reports whether this session's primary language server is still
// warming (handshake incomplete) and how long it has been. session_start uses it
// to soften "LSP is ready" into a warming advisory so an agent reaches for
// topology/workspace_symbols meanwhile instead of blocking a semantic tool on a cold
// server. Returns (false, 0) when no language is attached or the server is ready.
func (s *connSession) lspWarming() (bool, time.Duration) {
	if s.acquiredLanguageName() == "" {
		return false, 0
	}
	return s.sessionProxy.WarmupStatus("")
}

// routedLanguageNames returns the non-primary languages whose servers have
// actually served this session (empty when none have). daemon_info and
// session_start pair it with acquiredLanguageName so a connection with no
// primary — a LanguageNone root served purely by per-file routing — stops
// reporting that no language server is attached while one answers its queries.
func (s *connSession) routedLanguageNames() []string {
	if s.sessionProxy == nil {
		return nil
	}
	return s.sessionProxy.routedLanguages()
}

// lspDiagMode reports the resolved diagnostics mode of this session's primary
// language server (push / pull / hybrid / pull-requested-but-unavailable), or ""
// when no server is attached or the mode is not yet resolved. daemon_info and
// session_start surface it — the mode is authoritative negotiation state, never
// inferred from cache contents.
func (s *connSession) lspDiagMode() string {
	if s.acquiredLanguageName() == "" {
		return ""
	}
	return s.sessionProxy.DiagMode("")
}

// markBoundaryViolation records the violation on the session record and is
// deliberately sticky-not-terminating: each offending tool call already gets a
// WorkspaceBoundaryError back, which is the per-call enforcement contract.
// "Health: blocked" + HealthMessage is observability — the TUI surfaces it for
// the operator. NOTE the dashboard alert currently prints a FIXED string
// ("start a new MCP connection") rather than this message, which is wrong for a
// refused wide claim because reconnecting replays the same pin; only the
// session detail pane shows the text written here. Tracked separately — a
// message written here should still name its own remedy,
// while legitimate calls inside the pinned workspace keep working. We do not
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

// isStrict reports whether strict mode is in effect for this session.
func (s *connSession) isStrict() bool {
	return s.view().edits.Strict
}

// editsConfig returns the current resolved edits config.
func (s *connSession) editsConfig() config.EditsConfig {
	return s.view().edits
}

// memoryConfig returns the current resolved [memory] config off the lock-free
// snapshot (seeded at construction from global config, swapped per project on
// every attach / re-pin / reload). Lets the hot read_file hint path read the
// config without re-reading and re-parsing .plumb/config.toml per call.
func (s *connSession) memoryConfig() config.MemoryConfig {
	return s.view().memory
}

// collabConfig returns the connection's snapshotted, project-resolved [collab]
// config. Like memoryConfig it is captured on every attach / re-pin / reload, so
// the hot peer-hint path reads it without re-parsing .plumb/config.toml per call.
func (s *connSession) collabConfig() config.CollabConfig {
	return s.view().collab
}

// toolsConfig returns the current resolved [tools] config off the lock-free
// snapshot. Read on the tools/list filter path so the profile resolves without
// a per-call disk read; swapped per project like the blocks above.
func (s *connSession) toolsConfig() config.ToolsConfig {
	return s.view().tools
}

// refuseHomeRoots reports whether the session refuses home-directory roots.
func (s *connSession) refuseHomeRoots() bool {
	return s.view().walk.RefuseHomeRoots
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
