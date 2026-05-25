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
	cfg            config.Config
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

	watcherOnce sync.Once

	clientRequest mcp.RequestFn
	requestMu     sync.RWMutex
}

// newConnSession initialises a connSession and registers a new MCP session.
// Call close() when the connection ends.
func newConnSession(pool *workspacePool, topoPool *topologyPool, cfg config.Config, statsStore *statsStore, clientLimiters *sync.Map) *connSession {
	ttl := cfg.Cache.TTL.Duration
	sessName := session.GenerateName()
	sessID, _ := session.Register(session.Info{
		Name:          sessName,
		DaemonVersion: Version,
	})
	ctx, cancel := context.WithCancel(context.Background())
	return &connSession{
		ctx:            ctx,
		cancel:         cancel,
		pool:           pool,
		topologyPool:   topoPool,
		cfg:            cfg,
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
	}
}

// close releases per-session resources and unregisters the session.
func (s *connSession) close() {
	s.cancel()
	s.sessionCache.Close()
	if s.qualityRunner != nil {
		s.qualityRunner.Stop()
	}
	session.Unregister(s.sessID)
}

// workspace returns the resolved workspace root for the session.
func (s *connSession) workspace() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.acquiredRoot
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

	adapter := ""
	if language != LanguageNone {
		e, err := s.pool.acquireLang(ctx, folder, language)
		if err != nil {
			// LSP unavailable (binary not on PATH, crash, etc.) — degrade gracefully
			// rather than aborting. The workspace is still attached for filesystem
			// tools and stat tracking; LSP tools will surface their own errors.
			slog.Error("daemon: acquire LS — attaching without LSP", "root", folder, "language", language, "err", err)
			language = LanguageNone
		} else {
			s.sessionProxy.setPrimary(folder, e.proxy)
			s.sessionInv.setPrimary(folder, e.inv)
			switch language {
			case "go":
				adapter = "gopls"
			case "python":
				adapter = "pyright"
			}
		}
	}
	s.acquiredRoot = folder
	s.acquiredLanguage = language
	s.startQualityRunner(folder)
	s.startTopologyIndexer(folder)
	recoverWorkspaceTxlog(folder, txlog.Scan)
	cn, cv := s.clientName, s.clientVersion
	session.Patch(s.sessID, func(info *session.Info) {
		info.Folder = folder
		info.Language = language
		info.Adapter = adapter
		if cn != "" {
			info.ClientName = cn
			info.ClientVersion = cv
		}
	})
	slog.Info("daemon: session attached", "root", folder, "language", language, "adapter", adapter)
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
		info.Adapter = ""
		info.Synthetic = true
		if cn != "" {
			info.ClientName = cn
			info.ClientVersion = cv
		}
	})
	slog.Info("daemon: session auto-attached (synthetic)", "root", root)
}

// applyProjectConfig loads <workspace>/.plumb/config.toml and applies it to
// the live session (rate limit, strict mode, walk config).
func (s *connSession) applyProjectConfig(workspace string) {
	if workspace == "" {
		return
	}
	projectCfg, err := config.LoadProject(s.cfg, workspace)
	if err != nil {
		slog.Warn("daemon: project config invalid; using global", "workspace", workspace, "err", err)
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
	s.writeLimiter.SetLimit(projectCfg.Edits.RateLimitPerMinute)
	if projectCfg.Edits.Strict != s.cfg.Edits.Strict ||
		projectCfg.Edits.RateLimitPerMinute != s.cfg.Edits.RateLimitPerMinute ||
		projectCfg.Walk.RefuseHomeRoots != s.cfg.Walk.RefuseHomeRoots ||
		projectCfg.Git.AllowWrites != s.cfg.Git.AllowWrites ||
		projectCfg.Git.AllowDestructive != s.cfg.Git.AllowDestructive ||
		projectCfg.Git.AllowPush != s.cfg.Git.AllowPush {
		slog.Info("daemon: project config applied",
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
	slog.Info("daemon: project config hot-reloaded", "workspace", workspace)
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
	slog.Info("daemon: client identified", "client", name, "version", version)
	session.SetClient(s.sessID, name, version)
	if s.clientLimiters != nil {
		key := name + "/" + version
		shared, _ := s.clientLimiters.LoadOrStore(key,
			tools.NewRateLimiter(s.cfg.Edits.RateLimitPerMinute, time.Minute))
		s.writeLimiter.SetParent(shared.(*tools.RateLimiter))
	}
}

// onAfterTool records a completed tool call in the stats store.
func (s *connSession) onAfterTool(toolName string, args json.RawMessage, output, errMsg string, dur time.Duration, isError bool) {
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
		if !s.cfg.Workspace.AutoAttach {
			slog.Warn("daemon: cannot determine workspace root", "seed", "file://"+seedPath, "err", err)
			return
		}
		synthRoot := s.pool.SynthesiseRoot(startDir)
		s.attachSynthetic(toolCtx, synthRoot)
		if s.cfg.Workspace.AutoAttachPersist {
			go func() {
				if mkErr := materialisePlumbDir(synthRoot); mkErr != nil {
					slog.Warn("daemon: failed to materialise .plumb/", "root", synthRoot, "err", mkErr)
					return
				}
				slog.Info("daemon: materialised .plumb/ at synthetic root", "root", synthRoot)
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
	if !s.cfg.Topology.Enabled {
		return
	}
	if s.topologyPool == nil {
		return
	}
	s.topologyStore = s.topologyPool.Acquire(workspace)
}

// startQualityRunner creates and starts the quality runner when the [quality]
// block is enabled. Must be called under stateMu (it is called only during
// workspace attach while stateMu is held). No-op if already started.
func (s *connSession) startQualityRunner(workspace string) {
	if s.qualityRunner != nil {
		return
	}
	q := s.cfg.Quality
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

// buildWriteDeps assembles the WriteDeps struct used by all write tools.
func (s *connSession) buildWriteDeps() tools.WriteDeps {
	var qualityReport tools.QualityReportFn
	if r := s.qualityRunner; r != nil {
		qualityReport = r.Report
	}
	var topologyNotify tools.TopologyNotifyFn
	if store := s.topologyStore; store != nil {
		topologyNotify = func(path string) { store.Enqueue(path) }
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
		ConcurrentWriteSkewFn: func() time.Duration { return concurrentWriteSkew(s.editsConfig()) },
		WorkspaceFn:           s.workspace,
		ShowWriteDiffFn:       func() bool { return s.editsConfig().ShowWriteDiff },
		PostWriteNotifyFn:     s.javaPostWriteNotify,
		QualityReport:         qualityReport,
		TopologyNotify:        topologyNotify,
	}
}

// registerAllTools registers every MCP tool with srv.
func (s *connSession) registerAllTools(srv *mcp.Server, daemonStartedAt time.Time) {
	lspTimeout := s.cfg.LSPQuery.Timeout.Duration
	topoFn := func() *topology.Store {
		s.stateMu.Lock()
		defer s.stateMu.Unlock()
		return s.topologyStore
	}
	srv.Register(tools.NewFindSymbol(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout).WithTopologyFallback(topoFn))
	srv.Register(tools.NewWorkspaceSymbols(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout, s.workspace).WithTopologyFallback(topoFn))
	srv.Register(tools.NewGetDefinition(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout))
	srv.Register(tools.NewExplainSymbol(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout))
	srv.Register(tools.NewListSymbols(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout).WithTopologyFallback(topoFn))
	srv.Register(tools.NewFileOutline(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout).WithTopologyFallback(topoFn))
	srv.Register(tools.NewFindReferences(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout))
	srv.Register(tools.NewCallHierarchy(s.sessionProxy, lspTimeout))
	srv.Register(tools.NewTypeHierarchy(s.sessionProxy, lspTimeout))
	srv.Register(tools.NewDiagnosticsWithOpener(s.sessionInv, s.sessionProxy))
	srv.Register(tools.NewListFiles(s.workspace))
	srv.Register(tools.NewListDirectory(s.workspace))
	srv.Register(tools.NewReadFile(s.readTracker))
	srv.Register(tools.NewReadSymbol(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout, s.readTracker).WithTopologyFallback(topoFn))
	srv.Register(tools.NewReadMultipleFiles())
	wd := s.buildWriteDeps()
	srv.Register(tools.NewWriteFile(wd))
	srv.Register(tools.NewEditFile(wd))
	srv.Register(tools.NewDeleteFile(wd))
	srv.Register(tools.NewRenameFile(wd))
	srv.Register(tools.NewCopyFile(wd))
	srv.Register(tools.NewTransactionApply(wd))
	srv.Register(tools.NewSearchInFiles(s.workspace, s.sessionProxy, s.sessionCache, s.ttl))
	srv.Register(tools.NewFindFiles(s.workspace))
	srv.Register(tools.NewGit(wd, func() tools.GitPolicy {
		c := s.gitConfig()
		return tools.GitPolicy{
			AllowWrites:       c.AllowWrites,
			AllowDestructive:  c.AllowDestructive,
			AllowPush:         c.AllowPush,
			ProtectedBranches: c.ProtectedBranches,
		}
	}))
	srv.Register(tools.NewGitInit(wd))
	srv.Register(tools.NewFileDiff())
	srv.Register(tools.NewFindReplace(wd))
	srv.Register(tools.NewVersion())
	srv.Register(tools.NewDaemonInfoFunc(s.sessID, s.sessionName, Version, daemonStartedAt))
	srv.Register(tools.NewRenameSession(s.renameSession))
	srv.Register(tools.NewSessionStart(s.workspace, s.sessionInv, s.rootFromClient, s.refuseHomeRoots, s.clientNameStr).WithTopology(topoFn))
	srv.Register(tools.NewRenameSymbol(s.sessionProxy, lspTimeout))
	srv.Register(tools.NewInsertBeforeSymbol(s.sessionProxy, lspTimeout).WithTopologyFallback(topoFn))
	srv.Register(tools.NewInsertAfterSymbol(s.sessionProxy, lspTimeout).WithTopologyFallback(topoFn))
	srv.Register(tools.NewReplaceSymbolBody(s.sessionProxy, lspTimeout).WithTopologyFallback(topoFn))
	srv.Register(tools.NewSafeDeleteSymbol(s.sessionProxy, lspTimeout))
	srv.Register(tools.NewListMemories(s.workspace))
	srv.Register(tools.NewReadMemory(s.workspace))
	srv.Register(tools.NewWriteMemory(s.workspace))
	srv.Register(tools.NewDeleteMemory(s.workspace))
	srv.Register(tools.NewSearchMemories(s.workspace))
	srv.Register(tools.NewRelevantMemories(s.workspace))
	srv.Resources = memory.NewResourceProvider(s.workspace)
	srv.RegisterPrompt(mcp.NewOrientPrompt(s.workspace))
	srv.RegisterPrompt(mcp.NewWhatsBrokenPrompt(s.workspace))
	srv.RegisterPrompt(mcp.NewRecentChangesPrompt(s.workspace))
	srv.Register(tools.NewTopologyStatus(topoFn, s.workspace))
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
		slog.Info("daemon: roots changed — re-fetching workspace root")
		s.attachWorkspace(initCtx, rootFromRoots(initCtx, request))
		s.applyProjectConfig(s.workspace())
		s.startConfigWatcher()
	}
	srv.OnBeforeTool = func(toolCtx context.Context, name string, args json.RawMessage) {
		s.onBeforeTool(toolCtx, name, args)
	}
}
