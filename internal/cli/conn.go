package cli

// conn.go — per-connection MCP session state and behaviour.
//
// connSession holds the mutable state shared across all closures that serve
// one MCP connection. Methods on connSession host the bodies of what were
// previously anonymous closures inside handleConn, keeping handleConn itself
// a thin orchestrator (see daemon.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/mcp"
	"github.com/golimpio/plumb/internal/memory"
	"github.com/golimpio/plumb/internal/quality"
	"github.com/golimpio/plumb/internal/quality/golangcilint"
	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/stats"
	"github.com/golimpio/plumb/internal/tools"
	"github.com/golimpio/plumb/internal/tools/txlog"
	"github.com/golimpio/plumb/internal/topology"
)

// connSession holds all mutable per-connection state for an MCP session.
// All exported methods are safe for concurrent use.
type connSession struct {
	pool           *workspacePool
	store          *config.Store
	statsStore     *statsStore
	clientLimiters *sync.Map

	sessID   string
	sessName string

	ctx    context.Context
	cancel context.CancelFunc

	stateMu          sync.Mutex
	acquiredRoot     string
	acquiredLanguage string
	clientName       string
	clientVersion    string
	lastCfgMtime     time.Time

	sessionProxy *routingProxy
	sessionInv   *routingInvProxy
	sessionCache *cache.Cache
	readTracker  *tools.ReadTracker
	writeTracker *tools.WriteTracker
	ttl          time.Duration

	qualityRunner *quality.Runner
	topologyPool  *topologyPool
	topologyStore *topology.Store

	writeLimiter *tools.RateLimiter
	editsMu      sync.RWMutex
	editsCfg     config.EditsConfig
	walkMu       sync.RWMutex
	walkCfg      config.WalkConfig
	gitMu        sync.RWMutex
	gitCfg       config.GitConfig
	wsMu         sync.RWMutex
	wsCfg        config.WorkspaceConfig

	// boundary PathPolicy cache: rebuilt lazily when the pinned workspace
	// changes (policyRoot != workspace) and invalidated by applyProjectConfig
	// when configured roots change. depRoots* memoise the Go toolchain's
	// read-only dependency roots for the session lifetime. See boundary_policy.go.
	policyMu     sync.Mutex
	policy       *tools.PathPolicy
	policyRoot   string
	depRootsOnce sync.Once
	depRootsVal  []tools.AllowedRoot

	// applyMu serialises applyProjectConfig across the three paths that call it
	// (workspace attach, the 30s poll, and the global-config store subscription)
	// so their field swaps cannot interleave.
	applyMu sync.Mutex

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

// newConnSession initialises a connSession and registers a new MCP session.
// Call close() when the connection ends.
// The session context is derived from parent (the daemon context) so a
// daemon-wide shutdown cancels every session; s.cancel() additionally lets the
// idle reaper cancel one session in isolation. handleConn drives mcp.Serve on
// s.ctx, so either cancellation makes Serve return and the deferred cleanup run.
func newConnSession(parent context.Context, pool *workspacePool, topoPool *topologyPool, store *config.Store, statsStore *statsStore, clientLimiters *sync.Map) *connSession {
	cfg := store.Current()
	ttl := cfg.Cache.TTL.Duration
	sessName := session.GenerateName()
	sessID, _ := session.Register(session.Info{
		Name:          sessName,
		DaemonVersion: Version,
	})
	ctx, cancel := context.WithCancel(parent)
	s := &connSession{
		ctx:            ctx,
		cancel:         cancel,
		pool:           pool,
		topologyPool:   topoPool,
		store:          store,
		statsStore:     statsStore,
		clientLimiters: clientLimiters,
		sessID:         sessID,
		sessName:       sessName,
		ttl:            ttl,
		sessionProxy:   newRoutingProxy(pool),
		sessionInv:     newRoutingInvProxy(pool),
		sessionCache:   cache.New(ttl),
		readTracker:    tools.NewReadTracker(),
		writeTracker:   tools.NewWriteTracker(),
		writeLimiter:   tools.NewRateLimiter(cfg.Edits.RateLimitPerMinute, time.Minute),
		editsCfg:       cfg.Edits,
		walkCfg:        cfg.Walk,
		gitCfg:         cfg.Git,
		wsCfg:          cfg.Workspace,
		logger:         slog.Default().With("session_id", sessID),
	}
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
	if s.qualityRunner != nil {
		s.qualityRunner.Stop()
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
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.acquiredRoot
}

// acquiredLanguageName returns the LSP language attached to this session, or ""
// when none is (LanguageNone, or not yet attached). session_start uses it to
// distinguish a real "LSP is available" from a marker-detected project whose
// server is opt-in/off/missing — it must not advertise LSP tools that error.
func (s *connSession) acquiredLanguageName() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.acquiredLanguage == "" || s.acquiredLanguage == LanguageNone {
		return ""
	}
	return s.acquiredLanguage
}

// sessionName returns the current human-readable session name.
func (s *connSession) sessionName() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.sessName
}

// renameSession renames the session, persisting the new name in the session
// file and stats store.
func (s *connSession) renameSession(name string) (string, error) {
	name, err := session.Rename(s.sessID, name)
	if err != nil {
		return "", err
	}
	s.stateMu.Lock()
	s.sessName = name
	s.stateMu.Unlock()
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
	s.editsMu.RLock()
	defer s.editsMu.RUnlock()
	return s.editsCfg.Strict
}

// editsConfig returns the current resolved edits config.
func (s *connSession) editsConfig() config.EditsConfig {
	s.editsMu.RLock()
	defer s.editsMu.RUnlock()
	return s.editsCfg
}

// gitConfig returns the current resolved git tool config.
func (s *connSession) gitConfig() config.GitConfig {
	s.gitMu.RLock()
	defer s.gitMu.RUnlock()
	return s.gitCfg
}

// gitPolicy returns the connection's current resolved git policy. Reads the
// live gitCfg (hot-reloaded under its RWMutex) and is the single source of
// truth shared by the git tool's gate and session_start's policy report.
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
	s.walkMu.RLock()
	defer s.walkMu.RUnlock()
	return s.walkCfg.RefuseHomeRoots
}

// clientNameStr returns the MCP client name for the session.
func (s *connSession) clientNameStr() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.clientName
}

// setClientRequest stores the latest MCP RequestFn for subsequent rootsFn calls.
func (s *connSession) setClientRequest(req mcp.RequestFn) {
	s.requestMu.Lock()
	s.clientRequest = req
	s.requestMu.Unlock()
}

// attachWorkspace resolves rootURI to a project root, acquires the shared
// language server if needed, and updates the session record.
func (s *connSession) attachWorkspace(ctx context.Context, rootURI string) {
	folder := strings.TrimPrefix(rootURI, "file://")
	if folder == "" || folder == "/" {
		return
	}
	projectRoot, language, err := s.pool.Detect(folder)
	if err != nil {
		slog.Info("daemon: no project root found — deferring to first tool call", "folder", folder)
		return
	}
	if projectRoot != folder {
		folder = projectRoot
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.acquiredRoot != "" {
		return
	}

	detectedLanguage := language
	if detectedLanguage == LanguageNone {
		detectedLanguage = detectAnyLanguageAt(folder, s.store.Current())
	}
	adapter := ""
	if language != LanguageNone {
		e, err := s.pool.acquireLang(ctx, folder, language)
		if err != nil {
			// LSP unavailable (binary not on PATH, crash, etc.) — degrade gracefully
			// rather than aborting. The workspace is still attached for filesystem
			// tools and stat tracking; LSP tools will surface their own errors.
			s.log().Error("daemon: acquire LS — attaching without LSP", "root", folder, "language", language, "err", err)
			language = LanguageNone
		} else {
			s.sessionProxy.setPrimary(folder, e.proxy)
			s.sessionInv.setPrimary(folder, e.inv)
			adapter = adapterForLanguage(language)
		}
	}
	s.acquiredRoot = folder
	s.acquiredLanguage = language
	s.warmDepRoots(language)
	s.startQualityRunner(folder)
	s.startTopologyIndexer(folder)
	recoverWorkspaceTxlog(folder, txlog.Scan)
	cn, cv := s.clientName, s.clientVersion
	session.Patch(s.sessID, func(info *session.Info) {
		info.Folder = folder
		info.Language = language
		info.DetectedLanguage = detectedLanguage
		info.Adapter = adapter
		if cn != "" {
			info.ClientName = cn
			info.ClientVersion = cv
		}
	})
	s.log().Info("daemon: session attached", "root", folder, "language", language, "adapter", adapter)
}

// attachSynthetic records a synthetic workspace root when pool.Detect fails.
func (s *connSession) attachSynthetic(_ context.Context, root string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.acquiredRoot != "" {
		return
	}
	s.acquiredRoot = root
	s.startQualityRunner(root)
	s.startTopologyIndexer(root)
	recoverWorkspaceTxlog(root, txlog.Scan)
	cn, cv := s.clientName, s.clientVersion
	session.Patch(s.sessID, func(info *session.Info) {
		info.Folder = root
		info.Language = LanguageNone
		info.DetectedLanguage = detectAnyLanguageAt(root, s.store.Current())
		info.Adapter = ""
		info.Synthetic = true
		if cn != "" {
			info.ClientName = cn
			info.ClientVersion = cv
		}
	})
	s.log().Info("daemon: session auto-attached (synthetic)", "root", root)
}

// repinWorkspace deliberately switches the connection to a different workspace.
// Unlike attachWorkspace (idempotent, first-wins — the safe default for
// auto-resolution), this is driven only by an explicit session_start workspace
// argument: an unambiguous declaration of intent. It tears down the previous
// workspace's per-session subsystems (quality runner, topology store, LSP
// routing) and re-attaches the new root, so a connection reused across
// conversations (e.g. Claude Desktop) is no longer permanently welded to the
// first project it touched. The ad-hoc boundary guard on other path tools is
// unaffected — only this deliberate bootstrap call re-pins.
//
// folder may be any absolute path inside the target project. It is resolved to
// a workspace root via pool.Detect; when no marker is found the folder itself
// becomes the workspace (SynthesiseRoot), so an explicit pin always succeeds.
// Returns the resolved root.
func (s *connSession) repinWorkspace(ctx context.Context, folder string) (string, error) {
	folder = strings.TrimPrefix(folder, "file://")
	if folder == "" || folder == "/" {
		return "", fmt.Errorf("repin: empty workspace path %q", folder)
	}
	root, language, err := s.pool.Detect(folder)
	if err != nil {
		// No .plumb/marker/.git found — the folder itself becomes the workspace.
		root = s.pool.SynthesiseRoot(folder)
		language = LanguageNone
	}
	if s.attachOrRepinTo(ctx, root, language) {
		s.applyProjectConfig(root)
	}
	return root, nil
}

// onRootsChanged applies a client's updated workspace roots (the
// notifications/roots/list_changed path). On the first attach it pins the root,
// like OnInit. When the connection is already pinned and the client reports a
// different root — an editor that genuinely switched folders — it re-pins to
// follow the switch, closing the same "welded connection" gap that the
// session_start re-pin fixed for clients that never report roots (Claude
// Desktop). An empty or unchanged root is left alone: repinWorkspace no-ops when
// the resolved root matches the current pin, so a spurious notification (or a
// roots/list the client cannot satisfy) never tears the workspace down.
func (s *connSession) onRootsChanged(ctx context.Context, rootURI string) {
	s.stateMu.Lock()
	pinned := s.acquiredRoot != ""
	s.stateMu.Unlock()
	if !pinned {
		s.attachWorkspace(ctx, rootURI)
		s.applyProjectConfig(s.workspace())
		return
	}
	folder := strings.TrimPrefix(rootURI, "file://")
	if folder == "" || folder == "/" {
		return // client reported no usable root — keep the current pin
	}
	if _, err := s.repinWorkspace(ctx, folder); err != nil {
		s.log().Warn("daemon: roots-changed re-pin failed", "to", folder, "err", err)
	}
}

// attachOrRepinTo points the connection at root, tearing down any previous
// workspace's per-session subsystems first so the start* helpers (which no-op
// when already started) re-create them for the new root. Takes stateMu. Returns
// true when the root actually changed (false on a no-op re-pin to the same
// root). language is the LSP language for root, or LanguageNone.
func (s *connSession) attachOrRepinTo(ctx context.Context, root, language string) bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	prev := s.acquiredRoot
	if root == prev {
		return false
	}
	if s.qualityRunner != nil {
		s.qualityRunner.Stop()
		s.qualityRunner = nil
	}
	s.topologyStore = nil // pool stores are daemon-lifetime and shared; just re-Acquire
	// Per-session read/write tracking is workspace-relative: plumb has read and
	// written nothing in the new project yet, so the dirty-guard and strict-mode
	// read check must start clean rather than inherit the old root's paths.
	s.readTracker.Reset()
	s.writeTracker.Reset()

	adapter := ""
	if language != LanguageNone {
		if e, aerr := s.pool.acquireLang(ctx, root, language); aerr != nil {
			s.log().Error("daemon: re-pin acquire LS — attaching without LSP", "root", root, "language", language, "err", aerr)
			language = LanguageNone
		} else {
			s.sessionProxy.resetPrimary(root, e.proxy)
			s.sessionInv.resetPrimary(root, e.inv)
			adapter = adapterForLanguage(language)
		}
	}
	detectedLanguage := language
	if detectedLanguage == LanguageNone {
		detectedLanguage = detectAnyLanguageAt(root, s.store.Current())
	}
	s.acquiredRoot = root
	s.acquiredLanguage = language
	s.warmDepRoots(language)
	s.lastCfgMtime = time.Time{}
	s.startQualityRunner(root)
	s.startTopologyIndexer(root)
	recoverWorkspaceTxlog(root, txlog.Scan)
	cn, cv := s.clientName, s.clientVersion
	session.Patch(s.sessID, func(info *session.Info) {
		info.Folder = root
		info.Language = language
		info.DetectedLanguage = detectedLanguage
		info.Adapter = adapter
		info.Synthetic = false
		info.Health = ""
		info.HealthMessage = ""
		if cn != "" {
			info.ClientName = cn
			info.ClientVersion = cv
		}
	})
	s.log().Info("daemon: session re-pinned", "from", prev, "to", root, "language", language, "adapter", adapter)
	return true
}

// applyProjectConfig loads <workspace>/.plumb/config.toml and applies it to
// the live session (rate limit, strict mode, walk config).
func (s *connSession) applyProjectConfig(workspace string) {
	if workspace == "" {
		return
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	base := s.store.Current()
	projectCfg, err := config.LoadProject(base, workspace)
	if err != nil {
		s.log().Warn("daemon: project config invalid; using global", "workspace", workspace, "err", err)
		return
	}
	s.editsMu.Lock()
	s.editsCfg = projectCfg.Edits
	s.editsMu.Unlock()
	s.walkMu.Lock()
	s.walkCfg = projectCfg.Walk
	s.walkMu.Unlock()
	s.gitMu.Lock()
	s.gitCfg = projectCfg.Git
	s.gitMu.Unlock()
	s.wsMu.Lock()
	s.wsCfg = projectCfg.Workspace
	s.wsMu.Unlock()
	s.invalidateBoundaryPolicy()
	s.writeLimiter.SetLimit(projectCfg.Edits.RateLimitPerMinute)
	if projectCfg.Edits.Strict != base.Edits.Strict ||
		projectCfg.Edits.RateLimitPerMinute != base.Edits.RateLimitPerMinute ||
		projectCfg.Walk.RefuseHomeRoots != base.Walk.RefuseHomeRoots ||
		projectCfg.Git.AllowWrites != base.Git.AllowWrites ||
		projectCfg.Git.AllowDestructive != base.Git.AllowDestructive ||
		projectCfg.Git.AllowPush != base.Git.AllowPush {
		s.log().Info("daemon: project config applied",
			"workspace", workspace,
			"strict", projectCfg.Edits.Strict,
			"rate_limit_per_minute", projectCfg.Edits.RateLimitPerMinute,
			"refuse_home_roots", projectCfg.Walk.RefuseHomeRoots,
			"git.allow_writes", projectCfg.Git.AllowWrites,
			"git.allow_destructive", projectCfg.Git.AllowDestructive,
			"git.allow_push", projectCfg.Git.AllowPush)
	}
	configPath := filepath.Join(workspace, ".plumb", "config.toml")
	if info, err := os.Stat(configPath); err == nil {
		s.stateMu.Lock()
		s.lastCfgMtime = info.ModTime()
		s.stateMu.Unlock()
	}
	// The workspace is now known (attach / re-pin / reload all funnel here), so
	// link the per-(client, workspace) shared write budget. Idempotent.
	s.bindWriteLimiterParent()
}

// startConfigWatcher launches a background goroutine that polls for config file
// changes every 30 seconds and reapplies the config when the file is modified.
// The goroutine runs until s.ctx is cancelled (on session disconnect or daemon shutdown).
// Invoked exactly once per session via sync.Once.
func (s *connSession) startConfigWatcher() {
	s.watcherOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
					s.checkAndReloadConfig()
				}
			}
		}()
	})
}

// checkAndReloadConfig reapplies the workspace config when its file mtime
// differs from the last-applied version (lastCfgMtime, seeded at attach by
// applyProjectConfig). Any changed mtime triggers a reload — there is no
// staleness window, so edits made with a backdated mtime (git checkout,
// restore-from-backup) are still picked up. Called on each watcher poll.
func (s *connSession) checkAndReloadConfig() {
	workspace := s.workspace()
	if workspace == "" {
		return
	}
	configPath := filepath.Join(workspace, ".plumb", "config.toml")
	info, err := os.Stat(configPath)
	if err != nil {
		return
	}
	mtime := info.ModTime()
	s.stateMu.Lock()
	alreadySeen := mtime.Equal(s.lastCfgMtime)
	if !alreadySeen {
		s.lastCfgMtime = mtime
	}
	s.stateMu.Unlock()
	if alreadySeen {
		return
	}
	s.applyProjectConfig(workspace)
	s.log().Info("daemon: project config hot-reloaded", "workspace", workspace)
}

// javaPostWriteNotify sends DidOpen + DidClose to jdtls after a write so that
// it publishes fresh diagnostics. No-op for non-Java workspaces.
func (s *connSession) javaPostWriteNotify(ctx context.Context, path string) error {
	s.stateMu.Lock()
	lang := s.acquiredLanguage
	s.stateMu.Unlock()
	if lang != "java" {
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("java post-write notify: read %s: %w", path, err)
	}
	uri := protocol.FileURI(path)
	if err := s.sessionProxy.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri,
			LanguageID: "java",
			Version:    1,
			Text:       string(content),
		},
	}); err != nil {
		return fmt.Errorf("java post-write notify: DidOpen: %w", err)
	}
	return s.sessionProxy.DidClose(ctx, protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
}

// rootFromClient calls roots/list on the MCP client and resolves the first
// root URI to a workspace path via pool.Detect.
func (s *connSession) rootFromClient(ctx context.Context) string {
	s.requestMu.RLock()
	req := s.clientRequest
	s.requestMu.RUnlock()
	if req == nil {
		return ""
	}
	uri := rootFromRoots(ctx, req)
	if uri == "" {
		return ""
	}
	folder := strings.TrimPrefix(uri, "file://")
	if folder == "" || folder == "/" {
		return ""
	}
	root, _, err := s.pool.Detect(folder)
	if err != nil {
		return folder
	}
	return root
}

// onClientInfo handles the MCP clientInfo notification: stores client identity,
// updates the session record, and links the shared client rate-limiter budget.
func (s *connSession) onClientInfo(name, version string) {
	s.stateMu.Lock()
	s.clientName = name
	s.clientVersion = version
	s.stateMu.Unlock()
	s.log().Info("daemon: client identified", "client", name, "version", version)
	session.SetClient(s.sessID, name, version)
	// Client identity may arrive before or after the workspace is pinned; bind
	// here too so the shared budget links as soon as both are known.
	s.bindWriteLimiterParent()
}

// bindWriteLimiterParent links the session's write limiter to the budget shared
// by all connections from the same client identity working the SAME workspace.
//
// Keying on (client, workspace) — rather than client identity alone — preserves
// the anti-bypass guarantee within a project (a client cannot multiply its
// write budget by opening several connections to one workspace) while keeping
// different workspaces fully independent: a write burst in one project never
// throttles a sibling session in another. This is the cross-workspace isolation
// contract — two sessions on two different roots behave as isolated processes.
//
// No-op until both the client identity and the workspace are known. Writes
// cannot occur before a workspace is pinned (the boundary guard refuses them),
// so no shared budget is needed pre-attach. Idempotent: LoadOrStore + SetParent
// are safe to repeat, so it is called both on client-info and from
// applyProjectConfig (every attach / re-pin / config-reload path). On a re-pin
// SetParent atomically repoints this session at the new root's budget; the old
// root's shared limiter stays referenced by any sibling sessions still there.
func (s *connSession) bindWriteLimiterParent() {
	if s.clientLimiters == nil {
		return
	}
	s.stateMu.Lock()
	name, version, root := s.clientName, s.clientVersion, s.acquiredRoot
	s.stateMu.Unlock()
	if name == "" || root == "" {
		return
	}
	key := name + "/" + version + "\x00" + root
	shared, _ := s.clientLimiters.LoadOrStore(key,
		tools.NewRateLimiter(s.store.Current().Edits.RateLimitPerMinute, time.Minute))
	s.writeLimiter.SetParent(shared.(*tools.RateLimiter))
}

// onAfterTool records a completed tool call in the stats store and refreshes
// the session's last-seen timestamp so idle detection stays accurate.
func (s *connSession) onAfterTool(toolName string, args json.RawMessage, output, errMsg string, dur time.Duration, isError bool) {
	session.Touch(s.sessID)
	s.stateMu.Lock()
	root := s.acquiredRoot
	sessionName := s.sessName
	clientName := s.clientName
	clientVersion := s.clientVersion
	s.stateMu.Unlock()
	if w := workspaceFromArgs(s.pool, args); w != "" {
		root = w
	}
	if root == "" {
		return
	}
	s.statsStore.Record(root, stats.Call{
		SessionID:     s.sessID,
		SessionName:   sessionName,
		Tool:          toolName,
		CalledAt:      time.Now(),
		DurationMs:    dur.Milliseconds(),
		InputBytes:    len(args),
		OutputBytes:   len(output),
		Success:       !isError,
		ErrorMsg:      errMsg,
		InputJSON:     string(args),
		OutputText:    output,
		ClientName:    clientName,
		ClientVersion: clientVersion,
	})
}

// onBeforeTool resolves the workspace root from the tool arguments when the
// session has no primary workspace yet. Applies auto-attach and auto-attach-
// persist when configured.
func (s *connSession) onBeforeTool(toolCtx context.Context, _ string, args json.RawMessage) {
	s.stateMu.Lock()
	hasPrimary := s.acquiredRoot != ""
	s.stateMu.Unlock()
	if hasPrimary {
		return
	}
	seedPath := seedPathFromArgs(args)
	if seedPath == "" {
		return
	}
	startDir := seedPath
	if info, err := os.Stat(seedPath); err != nil || !info.IsDir() {
		startDir = filepath.Dir(seedPath)
	}
	root, _, err := s.pool.Detect(startDir)
	if err != nil {
		if !s.store.Current().Workspace.AutoAttach {
			s.log().Warn("daemon: cannot determine workspace root", "seed", "file://"+seedPath, "err", err)
			return
		}
		synthRoot := s.pool.SynthesiseRoot(startDir)
		s.attachSynthetic(toolCtx, synthRoot)
		if s.store.Current().Workspace.AutoAttachPersist {
			go func() {
				if mkErr := materialisePlumbDir(synthRoot); mkErr != nil {
					s.log().Warn("daemon: failed to materialise .plumb/", "root", synthRoot, "err", mkErr)
					return
				}
				s.log().Info("daemon: materialised .plumb/ at synthetic root", "root", synthRoot)
			}()
		}
		s.applyProjectConfig(s.workspace())
		s.startConfigWatcher()
		return
	}
	s.attachWorkspace(toolCtx, "file://"+root)
	s.applyProjectConfig(s.workspace())
	s.startConfigWatcher()
}

// startTopologyIndexer acquires the topology store for the workspace when
// topology is enabled. Must be called under stateMu. No-op if already started.
func (s *connSession) startTopologyIndexer(workspace string) {
	if s.topologyStore != nil {
		return
	}
	if s.topologyPool == nil {
		return
	}
	cfg := s.topologyConfigFor(workspace)
	if !cfg.Enabled {
		return
	}
	s.topologyStore = s.topologyPool.Acquire(workspace, cfg)
}

// topologyConfigFor returns the merged per-project [topology] config for
// workspace. LoadProject merges the project config
// (<workspace>/.plumb/config.toml) onto the global base, so per-project tuning
// (resync interval, batch, excludes, size caps) and an explicit enable/disable
// both win over the global default. Falls back to the global config when the
// project config cannot be read.
func (s *connSession) topologyConfigFor(workspace string) config.TopologyConfig {
	base := s.store.Current()
	cfg, err := config.LoadProject(base, workspace)
	if err != nil {
		return base.Topology
	}
	return cfg.Topology
}

// topologyEnabledFor reports whether topology indexing is enabled for workspace,
// honouring a per-project [topology] override (an opt-out wins over a global
// default-on, an opt-in over a global default-off).
func (s *connSession) topologyEnabledFor(workspace string) bool {
	return s.topologyConfigFor(workspace).Enabled
}

// topologyStoreLive returns the session's topology store, or nil when topology
// is disabled or the workspace has not yet attached. It reads under stateMu so
// it reflects a store attached after tool registration: registerAllTools — which
// builds the write-tool deps and the topology accessor — runs before the client
// handshake attaches the workspace.
func (s *connSession) topologyStoreLive() *topology.Store {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.topologyStore
}

// reconcileTopologyStore refreshes the session's topology store after a global
// config reload. The daemon-level subscriber (notified first — see config.Store's
// registration-order guarantee) runs topoPool.Reconcile, which may have closed or
// re-opened the pooled store for this root, leaving s.topologyStore on a closed
// handle; and a live enable/disable changes whether a store should exist at all.
// Re-acquiring (or clearing) here keeps the session's topology tools on a live
// store, so enabling/disabling topology takes effect on the current session, not
// only the next one. The project-config read happens before stateMu is taken.
func (s *connSession) reconcileTopologyStore(workspace string) {
	if s.topologyPool == nil {
		return
	}
	cfg := s.topologyConfigFor(workspace)
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if !cfg.Enabled {
		s.topologyStore = nil
		return
	}
	s.topologyStore = s.topologyPool.Acquire(workspace, cfg)
}

// startQualityRunner creates and starts the quality runner when the [quality]
// block is enabled. Must be called under stateMu (it is called only during
// workspace attach while stateMu is held). No-op if already started.
func (s *connSession) startQualityRunner(workspace string) {
	if s.qualityRunner != nil {
		return
	}
	q := s.store.Current().Quality
	if !q.Enabled {
		return
	}
	timeout := time.Duration(q.TimeoutMs) * time.Millisecond
	r := quality.NewRunner(quality.RunnerConfig{
		Workspace:          workspace,
		Analysers:          buildAnalysers(q.Analysers),
		Mode:               q.Mode,
		Timeout:            timeout,
		MaxFindingsPerFile: q.MaxFindingsPerFile,
	})
	r.Start()
	s.qualityRunner = r
}

// buildAnalysers constructs the Analyser list from the configured names.
// Unknown names are silently skipped.
func buildAnalysers(names []string) []quality.Analyser {
	out := make([]quality.Analyser, 0, len(names))
	for _, n := range names {
		switch n {
		case "golangci-lint":
			out = append(out, golangcilint.New())
		}
	}
	return out
}

func adapterForLanguage(language string) string {
	switch language {
	case "go":
		return "gopls"
	case "python":
		return "pyright"
	case "java":
		return "jdtls"
	case "rust":
		return "rust-analyzer"
	case "swift":
		return "sourcekit-lsp"
	case "zig":
		return "zls"
	case "typescript", "javascript":
		return "typescript-language-server"
	case "kotlin":
		return "kotlin-language-server"
	default:
		return ""
	}
}

func detectAnyLanguageAt(dir string, cfg config.Config) string {
	langs := make([]string, 0, len(cfg.LSP))
	for name, lspCfg := range cfg.LSP {
		if len(lspCfg.RootMarkers) > 0 {
			langs = append(langs, name)
		}
	}
	sort.Slice(langs, func(i, j int) bool {
		if langs[i] == "go" {
			return true
		}
		if langs[j] == "go" {
			return false
		}
		return langs[i] < langs[j]
	})
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		for _, name := range langs {
			for _, marker := range cfg.LSP[name].RootMarkers {
				if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
					return name
				}
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
	}
}

// buildWriteDeps assembles the WriteDeps struct used by all write tools.
func (s *connSession) buildWriteDeps() tools.WriteDeps {
	var qualityReport tools.QualityReportFn
	if r := s.qualityRunner; r != nil {
		qualityReport = r.Report
	}
	// Resolve the topology store lazily on each write: buildWriteDeps runs during
	// tool registration, before the client handshake attaches the workspace, so
	// capturing s.topologyStore eagerly here would always capture nil and silently
	// disable write-triggered re-indexing. Reading it per-write picks up the store
	// once the session attaches.
	topologyNotify := func(path string) {
		if store := s.topologyStoreLive(); store != nil {
			store.Enqueue(path)
		}
	}
	return tools.WriteDeps{
		Client:                s.sessionProxy,
		Cache:                 s.sessionCache,
		Diag:                  s.sessionInv,
		Limiter:               s.writeLimiter,
		Strict:                s.isStrict,
		Reads:                 s.readTracker,
		Writes:                s.writeTracker,
		PostWriteDiagWindowFn: func() time.Duration { return postWriteDiagWindow(s.editsConfig()) },
		DiagWait:              tools.NewDiagWaitEstimator(),
		ConcurrentWriteSkewFn: func() time.Duration { return concurrentWriteSkew(s.editsConfig()) },
		WorkspaceFn:           s.workspace,
		Boundary:              s.writeBoundaryGuard,
		ShowWriteDiffFn:       func() bool { return s.editsConfig().ShowWriteDiff },
		PostWriteNotifyFn:     s.javaPostWriteNotify,
		QualityReport:         qualityReport,
		TopologyNotify:        topologyNotify,
	}
}

// registerAllTools registers every MCP tool with srv.
func (s *connSession) registerAllTools(srv *mcp.Server, daemonStartedAt time.Time) {
	lspTimeout := s.store.Current().LSPQuery.Timeout.Duration
	topoFn := s.topologyStoreLive
	// Read tools (reads/searches) admit any allowed root including read-only
	// dependency roots; write/semantic-write tools demand read-write access.
	boundary := s.readBoundaryGuard
	writeBoundary := s.writeBoundaryGuard
	// The LSP routing proxies guard cross-workspace diagnostics queries, which
	// are reads.
	s.sessionProxy.setBoundaryGuard(boundary)
	s.sessionInv.setBoundaryGuard(boundary)
	srv.Register(tools.NewFindSymbol(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout).WithTopologyFallback(topoFn))
	srv.Register(tools.NewWorkspaceSymbols(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout, s.workspace).WithTopologyFallback(topoFn))
	srv.Register(tools.NewGetDefinition(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout))
	srv.Register(tools.NewExplainSymbol(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout))
	srv.Register(tools.NewListSymbols(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout).WithTopologyFallback(topoFn))
	srv.Register(tools.NewFileOutline(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout).WithTopologyFallback(topoFn).WithBoundary(boundary))
	srv.Register(tools.NewFindReferences(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout))
	srv.Register(tools.NewCallHierarchy(s.sessionProxy, lspTimeout))
	srv.Register(tools.NewTypeHierarchy(s.sessionProxy, lspTimeout))
	srv.Register(tools.NewDiagnosticsWithOpener(s.sessionInv, s.sessionProxy).WithBoundary(boundary))
	srv.Register(tools.NewListFiles(s.workspace).WithBoundary(boundary))
	srv.Register(tools.NewListDirectory(s.workspace).WithBoundary(boundary))
	srv.Register(tools.NewReadFile(s.readTracker).WithBoundary(boundary).WithClient(s.clientNameStr).WithOutsideLabel(s.outsideWorkspaceLabel))
	srv.Register(tools.NewReadSymbol(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout, s.readTracker).WithTopologyFallback(topoFn).WithBoundary(boundary).WithClient(s.clientNameStr).WithOutsideLabel(s.outsideWorkspaceLabel))
	srv.Register(tools.NewReadMultipleFiles().WithBoundary(boundary))
	wd := s.buildWriteDeps()
	srv.Register(tools.NewWriteFile(wd))
	srv.Register(tools.NewEditFile(wd))
	srv.Register(tools.NewDeleteFile(wd))
	srv.Register(tools.NewRenameFile(wd))
	srv.Register(tools.NewCopyFile(wd))
	srv.Register(tools.NewTransactionApply(wd))
	srv.Register(tools.NewSearchInFiles(s.workspace, s.sessionProxy, s.sessionCache, s.ttl).WithBoundary(boundary))
	srv.Register(tools.NewFindFiles(s.workspace).WithBoundary(boundary))
	srv.Register(tools.NewGit(wd, s.gitPolicy))
	srv.Register(tools.NewGitInit(wd))
	srv.Register(tools.NewFileDiff().WithBoundary(boundary))
	srv.Register(tools.NewFindReplace(wd))
	srv.Register(tools.NewVersion())
	srv.Register(tools.NewDaemonInfoFunc(s.sessID, s.sessionName, Version, daemonStartedAt).
		WithConfigStatus(func() tools.ConfigStatus {
			return tools.ConfigStatus{
				Generation:    s.store.Generation(),
				LastReloaded:  s.store.LastReloaded(),
				RestartNeeded: s.store.RestartNeeded(),
			}
		}))
	srv.Register(tools.NewRenameSession(s.renameSession))
	srv.Register(tools.NewSessionStart(s.workspace, s.sessionInv, s.rootFromClient, s.refuseHomeRoots, s.clientNameStr, s.gitPolicy).
		WithTopology(topoFn).
		WithLSPLanguage(s.acquiredLanguageName).
		WithRepin(s.repinWorkspace).
		WithPinConflict(func(requested string) {
			ws := s.workspace()
			s.markBoundaryViolation(fmt.Sprintf("session_start workspace switch refused: connection is pinned to %s; requested %s", ws, requested))
		}).
		WithExternalID(func(externalID string) string {
			session.SetExternalID(s.sessID, externalID)
			if prev := session.FindEnded(externalID, 24*time.Hour); prev != nil {
				if name, err := s.renameSession(prev.Name); err == nil {
					return name
				}
			}
			return ""
		}))
	srv.Register(tools.NewRenameSymbol(s.sessionProxy, lspTimeout).WithBoundary(writeBoundary))
	srv.Register(tools.NewInsertBeforeSymbol(s.sessionProxy, lspTimeout).WithTopologyFallback(topoFn))
	srv.Register(tools.NewInsertAfterSymbol(s.sessionProxy, lspTimeout).WithTopologyFallback(topoFn))
	srv.Register(tools.NewReplaceSymbolBody(s.sessionProxy, lspTimeout).WithTopologyFallback(topoFn))
	srv.Register(tools.NewSafeDeleteSymbol(s.sessionProxy, lspTimeout))
	srv.Register(tools.NewListMemories(s.workspace).WithBoundary(boundary))
	srv.Register(tools.NewReadMemory(s.workspace).WithBoundary(boundary))
	srv.Register(tools.NewWriteMemory(s.workspace).WithBoundary(boundary))
	srv.Register(tools.NewDeleteMemory(s.workspace).WithBoundary(boundary))
	srv.Register(tools.NewSearchMemories(s.workspace).WithBoundary(boundary))
	srv.Register(tools.NewRelevantMemories(s.workspace).WithBoundary(boundary))
	srv.Resources = memory.NewResourceProvider(s.workspace)
	srv.RegisterPrompt(mcp.NewOrientPrompt(s.workspace))
	srv.RegisterPrompt(mcp.NewWhatsBrokenPrompt(s.workspace))
	srv.RegisterPrompt(mcp.NewRecentChangesPrompt(s.workspace))
	srv.RegisterPrompt(mcp.NewSelftestPrompt(s.workspace))
	srv.Register(tools.NewTopologyStatus(topoFn, s.workspace).WithBoundary(boundary))
	srv.Register(tools.NewTopologySearch(topoFn))
	srv.Register(tools.NewTopologyExplore(topoFn))
	srv.Register(tools.NewTopologyImpact(topoFn))
	srv.Register(tools.NewTopologyAffected(topoFn))
	srv.Register(tools.NewTopologyRoutes(topoFn))
}

// registerHooks wires up the MCP lifecycle callbacks to connSession methods.
func (s *connSession) registerHooks(srv *mcp.Server) {
	srv.OnClientInfo = func(_ context.Context, name, version string) {
		s.onClientInfo(name, version)
	}
	srv.OnAfterTool = func(_ context.Context, toolName string, args json.RawMessage, output, errMsg string, dur time.Duration, isError bool) {
		s.onAfterTool(toolName, args, output, errMsg, dur, isError)
	}
	srv.OnInit = func(initCtx context.Context, request mcp.RequestFn) {
		s.setClientRequest(request)
		s.attachWorkspace(initCtx, rootFromRoots(initCtx, request))
		s.applyProjectConfig(s.workspace())
		s.startConfigWatcher()
	}
	srv.OnRootsChanged = func(initCtx context.Context, request mcp.RequestFn) {
		s.setClientRequest(request)
		s.log().Info("daemon: roots changed — re-fetching workspace root")
		s.onRootsChanged(initCtx, rootFromRoots(initCtx, request))
		s.startConfigWatcher()
	}
	srv.OnBeforeTool = func(toolCtx context.Context, name string, args json.RawMessage) {
		s.onBeforeTool(toolCtx, name, args)
	}
}
