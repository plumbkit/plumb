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
// conn_register.go. This file holds the session state, the copy-on-write
// mutation lane, and the lock-free accessors.

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/quality"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// sessionView is an immutable snapshot of a connSession's mutable state.
// Readers load it lock-free via connSession.state (an atomic.Pointer); mutators
// copy-on-write under muMutate (see mutate). A loaded *sessionView is treated as
// read-only — never mutate one in place.
type sessionView struct {
	acquiredRoot     string
	acquiredLanguage string
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
	lsRefLang     string
	clientName    string
	clientVersion string
	sessName      string
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
	tools     config.ToolsConfig
	// tasks holds the resolved [tasks.<lang>] command templates; agentConfigWrites
	// is the resolved enable knob for the agent-writable-config tool. Both are
	// swapped per project on every attach / re-pin / reload, like the blocks above.
	tasks             map[string]config.TasksConfig
	agentConfigWrites bool

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
	budgets    *sharedBudgets

	sessID string

	ctx    context.Context
	cancel context.CancelFunc

	state    atomic.Pointer[sessionView] // lock-free reads of the session snapshot
	muMutate sync.Mutex                  // the single mutation lane (see mutate)

	sessionProxy *routingProxy
	sessionInv   *routingInvProxy
	sessionCache *cache.Cache
	readTracker  *tools.ReadTracker
	writeTracker *tools.WriteTracker
	undoStore    *tools.UndoStore
	ttl          time.Duration

	topologyPool *topologyPool
	memoryPool   *memoryIndexPool
	hintCache    *memoryHintCache
	writeLimiter *tools.RateLimiter

	// hintSeen tracks the memory names already hinted on this connection, so a
	// memory is pointed out once per session, not on every read of a hot path.
	// Lazily created; cleared on re-pin.
	hintSeen   map[string]bool
	hintSeenMu sync.Mutex

	watcherOnce sync.Once
	unsubscribe func() // removes the store-change listener on close

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
func newConnSession(parent context.Context, pool *workspacePool, topoPool *topologyPool, store *config.Store, statsStore *statsStore, budgets *sharedBudgets) *connSession {
	cfg := store.Current()
	ttl := cfg.Cache.TTL.Duration
	sessName := session.GenerateName()
	sessID, _ := session.Register(session.Info{
		Name:          sessName,
		DaemonVersion: Version,
	})
	ctx, cancel := context.WithCancel(parent)
	s := &connSession{
		ctx:          ctx,
		cancel:       cancel,
		pool:         pool,
		topologyPool: topoPool,
		hintCache:    &memoryHintCache{},
		store:        store,
		statsStore:   statsStore,
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
	s.state.Store(&sessionView{
		sessName:  sessName,
		edits:     cfg.Edits,
		walk:      cfg.Walk,
		git:       cfg.Git,
		ws:        cfg.Workspace,
		semantics: cfg.Semantics,
		memory:    cfg.Memory,
		tools:     cfg.Tools,
	})
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
	session.Unregister(s.sessID)
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
// distinguish a real "LSP is available" from a marker-detected project whose
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
// to soften "LSP is available" into a warming advisory so an agent reaches for
// topology/find_symbol meanwhile instead of blocking a semantic tool on a cold
// server. Returns (false, 0) when no language is attached or the server is ready.
func (s *connSession) lspWarming() (bool, time.Duration) {
	if s.acquiredLanguageName() == "" {
		return false, 0
	}
	return s.sessionProxy.WarmupStatus("")
}

// sessionName returns the current human-readable session name.
func (s *connSession) sessionName() string {
	return s.view().sessName
}

// sessionPurpose returns the current session purpose tag ("" when unset).
func (s *connSession) sessionPurpose() string {
	return s.view().purpose
}

// setPurpose records a validated session purpose tag on the live session view
// and persists it to the session file so the TUI and workspace_sessions surface
// it. Subsequent stats rows for this session carry the tag.
func (s *connSession) setPurpose(purpose string) {
	s.mutate(func(v *sessionView) { v.purpose = purpose })
	session.SetPurpose(s.sessID, purpose)
}

// renameSession renames the session, persisting the new name in the session
// file and stats store.
func (s *connSession) renameSession(name string) (string, error) {
	name, err := session.Rename(s.sessID, name)
	if err != nil {
		return "", err
	}
	s.mutate(func(v *sessionView) { v.sessName = name })
	s.statsStore.RenameSession(s.sessID, name)
	return name, nil
}

// markBoundaryViolation records the violation on the session record and is
// deliberately sticky-not-terminating: each offending tool call already gets a
// WorkspaceBoundaryError back, which is the per-call enforcement contract.
// "Health: blocked" + HealthMessage is observability — the TUI and the
// dashboard alert ("start a new MCP connection") surface it for the operator,
// while legitimate calls inside the pinned workspace keep working. We do not
// cancel s.ctx here: a single confused tool call (e.g. an agent fumbling a
// path) should not tear down an otherwise-working session, and the boundary
// error is informative enough for the caller to course-correct.
func (s *connSession) markBoundaryViolation(message string) {
	if message == "" {
		return
	}
	session.Patch(s.sessID, func(info *session.Info) {
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

// toolsConfig returns the current resolved [tools] config off the lock-free
// snapshot. Read on the tools/list filter path so the profile resolves without
// a per-call disk read; swapped per project like the blocks above.
func (s *connSession) toolsConfig() config.ToolsConfig {
	return s.view().tools
}

// gitConfig returns the current resolved git tool config.
func (s *connSession) gitConfig() config.GitConfig {
	return s.view().git
}

// gitPolicy returns the connection's current resolved git policy. Reads the
// live git config off the lock-free snapshot (hot-reloaded via mutate) and is
// the single source of truth shared by the git tool's gate and session_start's
// policy report.
func (s *connSession) gitPolicy() tools.GitPolicy {
	c := s.gitConfig()
	return tools.GitPolicy{
		AllowWrites:       c.AllowWrites,
		AllowDestructive:  c.AllowDestructive,
		AllowPush:         c.AllowPush,
		ProtectedBranches: c.ProtectedBranches,
	}
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
