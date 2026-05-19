package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/mcp"
	"github.com/golimpio/plumb/internal/memory"
	"github.com/golimpio/plumb/internal/monitor"
	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/stats"
	"github.com/golimpio/plumb/internal/tools"
	"github.com/golimpio/plumb/internal/tools/txlog"
)

func recoverWorkspaceTxlog(folder string, scan func(string)) {
	scan(folder)
}

func postWriteDiagWindow(edits config.EditsConfig) time.Duration {
	if edits.PostWriteDiagnosticsMs == 0 {
		return -1
	}
	return time.Duration(edits.PostWriteDiagnosticsMs) * time.Millisecond
}

func concurrentWriteSkew(edits config.EditsConfig) time.Duration {
	return time.Duration(edits.ConcurrentWriteSkewMs) * time.Millisecond
}

var daemonCmd = &cobra.Command{
	Use:    "daemon",
	Short:  "Run the background daemon (usually started automatically by serve)",
	Hidden: true,
	RunE:   runDaemon,
}

func runDaemon(_ *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Acquire the daemon-lifetime lock before doing anything else. If another
	// daemon already holds it, exit immediately rather than stealing the socket
	// path from it (which is what `os.Remove(socketPath); net.Listen(...)`
	// below would otherwise do, leaving two daemons running on the same path).
	lock, err := acquireDaemonLock()
	if err != nil {
		if errors.Is(err, errDaemonAlreadyRunning) {
			slog.Info("daemon: another plumb daemon is already running — exiting")
			return nil
		}
		return err
	}
	defer lock.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	configLevel := cfg.LogLevel // saved for "plumb log-level reset"
	if err := setupLogging(configLevel, cfg.LogFormat); err != nil {
		slog.Warn("daemon: invalid log config; keeping defaults", "err", err)
	}

	hasEnabled := false
	for _, lspCfg := range cfg.LSP {
		if lspCfg.Enabled {
			hasEnabled = true
			break
		}
	}
	if !hasEnabled {
		return fmt.Errorf("no language servers enabled; edit ~/.config/plumb/config.toml")
	}

	socketPath := daemonSocketPath()
	_ = os.Remove(socketPath)
	_ = os.MkdirAll(filepath.Dir(socketPath), 0o700)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("daemon: listening on %s: %w", socketPath, err)
	}
	defer os.Remove(socketPath)
	defer ln.Close()

	pidPath := daemonPIDPath()
	if err := os.WriteFile(pidPath, fmt.Appendf(nil, "%d", os.Getpid()), 0o644); err != nil {
		slog.Warn("daemon: could not write PID file", "path", pidPath, "err", err)
	}
	defer os.Remove(pidPath)

	// Publish our build version next to the PID so `plumb serve` can detect a
	// version mismatch (running daemon older than the binary that's launching).
	versionPath := daemonVersionPath()
	if err := os.WriteFile(versionPath, []byte(Version), 0o644); err != nil {
		slog.Warn("daemon: could not write version file", "path", versionPath, "err", err)
	}
	defer os.Remove(versionPath)

	statsStore := newStatsStore()
	defer statsStore.Close()

	pool := newWorkspacePool(cfg)
	defer pool.close()

	ctrlPath := daemonCtrlSocketPath()
	_ = os.Remove(ctrlPath)
	ctrlLn, ctrlErr := net.Listen("unix", ctrlPath)
	if ctrlErr != nil {
		slog.Warn("daemon: could not start control socket", "path", ctrlPath, "err", ctrlErr)
	} else {
		defer os.Remove(ctrlPath)
		defer ctrlLn.Close()
		go func() {
			<-ctx.Done()
			ctrlLn.Close()
		}()
		diagsFn := func(workspace string) string {
			e := pool.lookup(workspace)
			if e == nil {
				return ""
			}
			return tools.FormatDiagnostics(e.inv.AllDiagnostics())
		}
		go serveControlSocket(ctrlLn, configLevel, cfg.LogFormat, diagsFn)
	}

	daemonStartedAt := time.Now()
	slog.Info("daemon: ready", "socket", socketPath, "pid", os.Getpid(), "log", daemonLogPath())

	tools.Version = Version

	// Start the background LRU sweep for per-path write locks. Runs for the
	// daemon's lifetime; ctx cancellation stops the sweep goroutine cleanly.
	tools.StartPathLockSweep(ctx)
	monitor.StartSnapshotWriter(ctx, monitor.SnapshotPath(), 2*time.Second)

	// clientLimiters holds one RateLimiter per MCP client identity
	// (ClientName+"/"+ClientVersion). Connections from the same client share
	// this budget so opening multiple connections cannot multiply the allowed
	// write rate.
	var clientLimiters sync.Map // map[string]*tools.RateLimiter

	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				wg.Wait()
				return nil
			default:
				slog.Error("daemon: accept", "err", err)
				continue
			}
		}
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("daemon: connection goroutine panic — daemon kept alive",
						"err", r,
						"stack", string(debug.Stack()))
				}
			}()
			handleConn(ctx, conn, pool, cfg, statsStore, daemonStartedAt, &clientLimiters)
		})
	}
}

// handleConn runs a complete MCP session over conn, attaching to a shared
// gopls process from pool once the workspace root is determined.
func handleConn(ctx context.Context, conn net.Conn, pool *workspacePool, cfg config.Config, statsStore *statsStore, daemonStartedAt time.Time, clientLimiters *sync.Map) {
	defer conn.Close()

	// Register the session immediately so it appears in `plumb sessions` and the
	// TUI as soon as the client connects — before the workspace is resolved.
	// Language/adapter are filled in later by attachWorkspace once the workspace
	// is detected (Go, Python, or "none" for an LSP-less .plumb/ workspace).
	// Empty here so the TUI shows "(resolving...)" rather than mis-claiming
	// "gopls" for what might turn out to be a Python or no-LSP project.
	sessName := session.GenerateName()
	sessID, _ := session.Register(session.Info{
		Name:          sessName,
		DaemonVersion: Version,
	})
	defer session.Unregister(sessID)

	// Multi-workspace aware proxies. Each LSP tool call routes to the gopls
	// for the workspace containing its URI; diagnostics route the same way.
	// Workspace-wide methods (WorkspaceSymbols, Initialize, etc.) fall back
	// to the connection's primary workspace.
	sessionProxy := newRoutingProxy(pool)
	sessionInv := newRoutingInvProxy(pool)
	var acquiredRoot string
	var acquiredLanguage string
	var clientName, clientVersion string
	var stateMu sync.Mutex

	// attachWorkspace resolves the workspace root from rootURI, registers it
	// against the session, and (when the project has an enabled LSP language)
	// acquires the shared language server. For LanguageNone workspaces — a
	// `.plumb/` marker without a Go/Python root — the LSP step is skipped but
	// everything else (session.Folder, stats attribution, project config)
	// still applies. The previous name `startGopls` undersold this: it has
	// handled pyright since 0.5.x and now handles no-LSP workspaces too.
	attachWorkspace := func(startCtx context.Context, rootURI string) {
		folder := strings.TrimPrefix(rootURI, "file://")
		if folder == "" || folder == "/" {
			return
		}
		projectRoot, language, err := pool.Detect(folder)
		if err != nil {
			slog.Info("daemon: no project root found — deferring to first tool call", "folder", folder)
			return
		}
		if projectRoot != folder {
			folder = projectRoot
			rootURI = "file://" + folder
		}

		stateMu.Lock()
		defer stateMu.Unlock()
		if acquiredRoot != "" {
			return // already resolved for this session
		}

		adapter := ""
		if language != LanguageNone {
			e, err := pool.acquireLang(startCtx, folder, language)
			if err != nil {
				slog.Error("daemon: acquire LS", "root", folder, "language", language, "err", err)
				return
			}
			sessionProxy.setPrimary(folder, e.proxy)
			sessionInv.setPrimary(folder, e.inv)
			switch language {
			case "go":
				adapter = "gopls"
			case "python":
				adapter = "pyright"
			}
		}
		acquiredRoot = folder
		acquiredLanguage = language

		// Scan for orphaned transaction logs from a previous daemon crash and
		// roll them back before any new transactions can touch those files.
		recoverWorkspaceTxlog(folder, txlog.Scan)

		// Update the session file with the now-resolved workspace.
		cn, cv := clientName, clientVersion
		session.Patch(sessID, func(info *session.Info) {
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

	ttl := cfg.Cache.TTL.Duration
	sessionCache := cache.New(ttl)
	defer sessionCache.Close()

	// Workspace getter shared by tools that need to know the connection's
	// primary workspace (memory tools, symbol-filter tools).
	wsFn := func() string {
		stateMu.Lock()
		defer stateMu.Unlock()
		return acquiredRoot
	}
	nameFn := func() string {
		stateMu.Lock()
		defer stateMu.Unlock()
		return sessName
	}
	renameSessionFn := func(name string) (string, error) {
		name, err := session.Rename(sessID, name)
		if err != nil {
			return "", err
		}
		stateMu.Lock()
		sessName = name
		stateMu.Unlock()
		statsStore.RenameSession(sessID, name)
		return name, nil
	}

	// clientRequest is the latest RequestFn captured from OnInit. Tools
	// that need to talk to the MCP client (e.g. session_start calling
	// roots/list when the workspace hasn't been resolved yet) use this.
	var clientRequest mcp.RequestFn
	var requestMu sync.RWMutex
	// rootsFn returns the first workspace root reported by the client via
	// roots/list, attempting workspace resolution via pool.Detect. Returns
	// "" if the client doesn't support roots/list or none can be resolved.
	rootsFn := func(rootsCtx context.Context) string {
		requestMu.RLock()
		req := clientRequest
		requestMu.RUnlock()
		if req == nil {
			return ""
		}
		uri := rootFromRoots(rootsCtx, req)
		if uri == "" {
			return ""
		}
		folder := strings.TrimPrefix(uri, "file://")
		if folder == "" || folder == "/" {
			return ""
		}
		root, _, err := pool.Detect(folder)
		if err != nil {
			return folder // best-effort: caller may still find it useful
		}
		return root
	}

	srv := mcp.New(mcp.ServerInfo{Name: "plumb", Version: Version})
	srv.Register(tools.NewFindSymbol(sessionProxy, sessionCache, ttl))
	srv.Register(tools.NewWorkspaceSymbols(sessionProxy, sessionCache, ttl, wsFn))
	srv.Register(tools.NewGetDefinition(sessionProxy, sessionCache, ttl))
	srv.Register(tools.NewExplainSymbol(sessionProxy, sessionCache, ttl))
	srv.Register(tools.NewListSymbols(sessionProxy, sessionCache, ttl))
	srv.Register(tools.NewFindReferences(sessionProxy, sessionCache, ttl))
	srv.Register(tools.NewCallHierarchy(sessionProxy))
	srv.Register(tools.NewTypeHierarchy(sessionProxy))
	srv.Register(tools.NewDiagnostics(sessionInv))
	srv.Register(tools.NewListFiles())
	srv.Register(tools.NewListDirectory())
	readTracker := tools.NewReadTracker()
	srv.Register(tools.NewReadFile(readTracker))
	srv.Register(tools.NewReadSymbol(sessionProxy, sessionCache, ttl, readTracker))
	srv.Register(tools.NewReadMultipleFiles())
	// Initial limit from the global config; updated to per-project values
	// inside applyProjectConfig once the workspace resolves.
	writeLimiter := tools.NewRateLimiter(cfg.Edits.RateLimitPerMinute, time.Minute)
	// editsCfg is updated by applyProjectConfig when project-local config
	// loads. Write tools consult it via closures so changes take effect on
	// the next call.
	var editsMu sync.RWMutex
	editsCfg := cfg.Edits
	strictFn := func() bool {
		editsMu.RLock()
		defer editsMu.RUnlock()
		return editsCfg.Strict
	}
	editConfigFn := func() config.EditsConfig {
		editsMu.RLock()
		defer editsMu.RUnlock()
		return editsCfg
	}
	// walkCfg mirrors editsCfg's pattern: updated on project-config reload,
	// read by session_start before every directory walk.
	var walkMu sync.RWMutex
	walkCfg := cfg.Walk
	refuseHomeRootsFn := func() bool {
		walkMu.RLock()
		defer walkMu.RUnlock()
		return walkCfg.RefuseHomeRoots
	}
	// applyProjectConfig is invoked after the workspace resolves; it loads
	// <workspace>/.plumb/config.toml, merges it onto the global config, and
	// applies relevant settings to the live session (rate limit, strict
	// mode). Safe to call multiple times if roots change.
	applyProjectConfig := func(workspace string) {
		if workspace == "" {
			return
		}
		projectCfg, err := config.LoadProject(cfg, workspace)
		if err != nil {
			slog.Warn("daemon: project config invalid; using global", "workspace", workspace, "err", err)
			return
		}
		editsMu.Lock()
		editsCfg = projectCfg.Edits
		editsMu.Unlock()
		walkMu.Lock()
		walkCfg = projectCfg.Walk
		walkMu.Unlock()
		writeLimiter.SetLimit(projectCfg.Edits.RateLimitPerMinute)
		if projectCfg.Edits.Strict != cfg.Edits.Strict || projectCfg.Edits.RateLimitPerMinute != cfg.Edits.RateLimitPerMinute || projectCfg.Walk.RefuseHomeRoots != cfg.Walk.RefuseHomeRoots {
			slog.Info("daemon: project config applied",
				"workspace", workspace,
				"strict", projectCfg.Edits.Strict,
				"rate_limit_per_minute", projectCfg.Edits.RateLimitPerMinute,
				"refuse_home_roots", projectCfg.Walk.RefuseHomeRoots)
		}
	}
	// javaPostWriteNotify implements DidOpen + DidClose after writes on Java
	// workspaces. jdtls only publishes diagnostics for open documents, so
	// DidChangeWatchedFiles alone is not enough to trigger fresh diagnostics.
	javaPostWriteNotify := func(ctx context.Context, path string) error {
		stateMu.Lock()
		lang := acquiredLanguage
		stateMu.Unlock()
		if lang != "java" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("java post-write notify: read %s: %w", path, err)
		}
		uri := protocol.FileURI(path)
		if err := sessionProxy.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{
				URI:        uri,
				LanguageID: "java",
				Version:    1,
				Text:       string(content),
			},
		}); err != nil {
			return fmt.Errorf("java post-write notify: DidOpen: %w", err)
		}
		return sessionProxy.DidClose(ctx, protocol.DidCloseTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		})
	}
	writeDeps := tools.WriteDeps{
		Client:                sessionProxy,
		Cache:                 sessionCache,
		Diag:                  sessionInv,
		Limiter:               writeLimiter,
		Strict:                strictFn,
		Reads:                 readTracker,
		PostWriteDiagWindowFn: func() time.Duration { return postWriteDiagWindow(editConfigFn()) },
		ConcurrentWriteSkewFn: func() time.Duration { return concurrentWriteSkew(editConfigFn()) },
		WorkspaceFn:           wsFn,
		ShowWriteDiffFn:       func() bool { return editConfigFn().ShowWriteDiff },
		PostWriteNotifyFn:     javaPostWriteNotify,
	}
	srv.Register(tools.NewWriteFile(writeDeps))
	srv.Register(tools.NewEditFile(writeDeps))
	srv.Register(tools.NewDeleteFile(writeDeps))
	srv.Register(tools.NewRenameFile(writeDeps))
	srv.Register(tools.NewTransactionApply(writeDeps))
	srv.Register(tools.NewSearchInFiles())
	srv.Register(tools.NewFindFiles())
	srv.Register(tools.NewGit())
	srv.Register(tools.NewFileDiff())
	srv.Register(tools.NewFindReplace(writeDeps))
	srv.Register(tools.NewVersion())
	srv.Register(tools.NewDaemonInfoFunc(sessID, nameFn, Version, daemonStartedAt))
	srv.Register(tools.NewRenameSession(renameSessionFn))
	clientNameFn := func() string {
		stateMu.Lock()
		defer stateMu.Unlock()
		return clientName
	}
	srv.Register(tools.NewSessionStart(wsFn, sessionInv, rootsFn, refuseHomeRootsFn, clientNameFn))

	// Edit tools — LSP-semantic refactoring + body replacement / inserts.
	srv.Register(tools.NewRenameSymbol(sessionProxy))
	srv.Register(tools.NewInsertBeforeSymbol(sessionProxy))
	srv.Register(tools.NewInsertAfterSymbol(sessionProxy))
	srv.Register(tools.NewReplaceSymbolBody(sessionProxy))
	srv.Register(tools.NewSafeDeleteSymbol(sessionProxy))

	// Memory tools — reuse the same workspace getter defined above.
	srv.Register(tools.NewListMemories(wsFn))
	srv.Register(tools.NewReadMemory(wsFn))
	srv.Register(tools.NewWriteMemory(wsFn))
	srv.Register(tools.NewDeleteMemory(wsFn))
	srv.Register(tools.NewSearchMemories(wsFn))
	srv.Register(tools.NewRelevantMemories(wsFn))

	// Expose memories as MCP resources so Claude Desktop's resources panel
	// surfaces them as browseable artifacts.
	srv.Resources = memory.NewResourceProvider(wsFn)

	// Register built-in prompts — surfaced as buttons/menu items in Claude Desktop.
	srv.RegisterPrompt(mcp.NewOrientPrompt(wsFn))
	srv.RegisterPrompt(mcp.NewWhatsBrokenPrompt(wsFn))
	srv.RegisterPrompt(mcp.NewRecentChangesPrompt(wsFn))

	srv.OnClientInfo = func(_ context.Context, name, version string) {
		stateMu.Lock()
		clientName = name
		clientVersion = version
		stateMu.Unlock()
		slog.Info("daemon: client identified", "client", name, "version", version)
		// Always update: sessID is registered immediately on connection.
		session.SetClient(sessID, name, version)

		// Attach the daemon-scoped client budget as the parent of this
		// connection's per-connection limiter. All connections from the same
		// client share one budget, preventing limit bypass by opening multiple
		// MCP connections. The per-connection limiter remains independent so
		// per-project config changes (applyProjectConfig → SetLimit) are
		// isolated to this connection.
		if clientLimiters != nil {
			key := name + "/" + version
			shared, _ := clientLimiters.LoadOrStore(key,
				tools.NewRateLimiter(cfg.Edits.RateLimitPerMinute, time.Minute))
			writeLimiter.SetParent(shared.(*tools.RateLimiter))
		}
	}

	srv.OnAfterTool = func(_ context.Context, toolName string, args json.RawMessage, output, errMsg string, dur time.Duration, isError bool) {
		// The global stats DB stores project identity per row. Calls without a
		// discoverable workspace are dropped because they cannot be attributed.
		stateMu.Lock()
		root := acquiredRoot
		sessionName := sessName
		stateMu.Unlock()
		if w := workspaceFromArgs(pool, args); w != "" {
			root = w
		}
		if root == "" {
			return
		}
		statsStore.Record(root, stats.Call{
			SessionID:   sessID,
			SessionName: sessionName,
			Tool:        toolName,
			CalledAt:    time.Now(),
			DurationMs:  dur.Milliseconds(),
			InputBytes:  len(args),
			OutputBytes: len(output),
			Success:     !isError,
			ErrorMsg:    errMsg,
			InputJSON:   string(args),
			OutputText:  output,
		})
	}

	srv.OnInit = func(initCtx context.Context, request mcp.RequestFn) {
		requestMu.Lock()
		clientRequest = request
		requestMu.Unlock()
		rootURI := rootFromRoots(initCtx, request)
		attachWorkspace(initCtx, rootURI)
		applyProjectConfig(wsFn())
	}

	srv.OnRootsChanged = func(initCtx context.Context, request mcp.RequestFn) {
		requestMu.Lock()
		clientRequest = request
		requestMu.Unlock()
		slog.Info("daemon: roots changed — re-fetching workspace root")
		rootURI := rootFromRoots(initCtx, request)
		attachWorkspace(initCtx, rootURI)
		applyProjectConfig(wsFn())
	}

	// attachSynthetic records a synthetic workspace root for sessions where
	// Detect failed (no project marker found). The session is fully attributed
	// — Folder, Language=none, Synthetic=true — so stats and TUI work normally;
	// only LSP tools are unavailable. If AutoAttachPersist is true the caller
	// is responsible for materialising the .plumb/ directory.
	attachSynthetic := func(_ context.Context, root string) {
		stateMu.Lock()
		defer stateMu.Unlock()
		if acquiredRoot != "" {
			return
		}
		acquiredRoot = root
		recoverWorkspaceTxlog(root, txlog.Scan)
		cn, cv := clientName, clientVersion
		session.Patch(sessID, func(info *session.Info) {
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

	srv.OnBeforeTool = func(toolCtx context.Context, _ string, args json.RawMessage) {
		stateMu.Lock()
		hasPrimary := acquiredRoot != ""
		stateMu.Unlock()
		if hasPrimary {
			return
		}
		seedPath := seedPathFromArgs(args)
		if seedPath == "" {
			return
		}
		// If seedPath is already a directory (filesystem tools pass the search
		// root, not a file), use it directly. filepath.Dir on a directory path
		// strips the last component and would miss the project root marker.
		startDir := seedPath
		if info, err := os.Stat(seedPath); err != nil || !info.IsDir() {
			startDir = filepath.Dir(seedPath)
		}
		root, _, err := pool.Detect(startDir)
		if err != nil {
			if !cfg.Workspace.AutoAttach {
				slog.Warn("daemon: cannot determine workspace root", "seed", "file://"+seedPath, "err", err)
				return
			}
			synthRoot := pool.SynthesiseRoot(startDir)
			attachSynthetic(toolCtx, synthRoot)
			if cfg.Workspace.AutoAttachPersist {
				go func() {
					plumbDir := filepath.Join(synthRoot, ".plumb")
					if mkErr := os.MkdirAll(plumbDir, 0o755); mkErr != nil {
						slog.Warn("daemon: failed to materialise .plumb/", "root", synthRoot, "err", mkErr)
						return
					}
					slog.Info("daemon: materialised .plumb/ at synthetic root", "root", synthRoot)
				}()
			}
			applyProjectConfig(wsFn())
			return
		}
		attachWorkspace(toolCtx, "file://"+root)
		applyProjectConfig(wsFn())
	}

	_ = srv.Serve(ctx, conn, conn)
}

// daemonSocketPath returns the Unix socket path for the plumb daemon.
func daemonSocketPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.sock")
}

// daemonCtrlSocketPath returns the Unix socket path for daemon admin commands
// (log level changes). Separate from the MCP socket so it never appears in the
// tool list and cannot be reached by MCP clients.
func daemonCtrlSocketPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.ctrl.sock")
}

// daemonPIDPath returns the path where the daemon writes its PID.
func daemonPIDPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.pid")
}

// daemonVersionPath returns the path where the daemon publishes its build
// version (read by `plumb serve` to detect a stale daemon).
func daemonVersionPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.version")
}

// plumbRuntimeDir returns the directory used for daemon runtime files
// (socket, PID). It uses os.UserCacheDir so the path is stable and consistent
// regardless of how the process was launched — critical on macOS where
// os.TempDir() follows $TMPDIR, which differs between GUI apps and terminals.
func plumbRuntimeDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		// os.UserCacheDir only fails if $HOME is unset; fall back to os.TempDir
		// which is the best we can do in that degenerate case.
		base = os.TempDir()
	}
	dir := filepath.Join(base, "plumb")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// startDaemonProcess launches a detached plumb daemon subprocess.
// Logs are written to daemonLogPath(); the process is detached with Setsid so
// it outlives the calling plumb serve process.
func startDaemonProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	logPath := daemonLogPath()
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logFile, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}

	cmd := exec.Command(exe, "daemon")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// daemonLogPath returns the OS-appropriate path for daemon log output.
//   - macOS : ~/Library/Logs/plumb/daemon.log
//   - Linux : $XDG_STATE_HOME/plumb/daemon.log  (fallback: ~/.local/state/plumb/daemon.log)
//   - other : $TMPDIR/plumb/daemon.log
func daemonLogPath() string {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			break
		}
		return filepath.Join(home, "Library", "Logs", "plumb", "daemon.log")
	case "linux":
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			return filepath.Join(xdg, "plumb", "daemon.log")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			break
		}
		return filepath.Join(home, ".local", "state", "plumb", "daemon.log")
	}
	return filepath.Join(os.TempDir(), "plumb", "daemon.log")
}

// rootFromRoots calls roots/list on the MCP client and returns the first root
// URI, or "" if the client does not support roots/list or returns no roots.
func rootFromRoots(ctx context.Context, request mcp.RequestFn) string {
	raw, err := request(ctx, "roots/list", nil)
	if err != nil {
		slog.Info("roots/list not supported by client — deferring to OnBeforeTool", "err", err)
		return ""
	}

	var resp struct {
		Roots []struct {
			URI string `json:"uri"`
		} `json:"roots"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		slog.Warn("parsing roots/list response", "err", err)
		return ""
	}
	if len(resp.Roots) == 0 {
		slog.Info("roots/list returned no roots — deferring to OnBeforeTool")
		return ""
	}

	root := resp.Roots[0].URI
	slog.Info("workspace root from MCP client", "rootURI", root)
	return root
}

// workspaceFromArgs returns the resolved workspace root for a tool call's raw
// JSON arguments. Returns "" if no path-bearing field is present or the path
// doesn't sit under a discoverable project root.
func workspaceFromArgs(pool *workspacePool, args json.RawMessage) string {
	seed := seedPathFromArgs(args)
	if seed == "" {
		return ""
	}
	// If seed is already a directory, use it directly — filepath.Dir would
	// strip the last component and miss the project root marker.
	startDir := seed
	if info, err := os.Stat(seed); err != nil || !info.IsDir() {
		startDir = filepath.Dir(seed)
	}
	root, _, err := pool.Detect(startDir)
	if err != nil {
		return ""
	}
	return root
}

// seedPathFromArgs extracts a single filesystem path from a tool call's raw
// JSON arguments. Probes the argument shapes plumb's tools use:
//
//	{"uri": "file:///..."}                      — LSP tools
//	{"path": "/..."}                            — most filesystem tools
//	{"root": "/..."}                            — list_files
//	{"workspace": "/..."}                       — session_start
//	{"paths": ["/...", ...]}                    — read_multiple_files
//	{"operations": [{"path": "/..."}, ...]}     — transaction_apply
//
// Returns "" if no shape matches. Any leading file:// is stripped so the
// caller gets a plain filesystem path.
func seedPathFromArgs(args json.RawMessage) string {
	var a struct {
		URI        string   `json:"uri"`
		Path       string   `json:"path"`
		Root       string   `json:"root"`
		Workspace  string   `json:"workspace"`
		Paths      []string `json:"paths"`
		Operations []struct {
			Path string `json:"path"`
		} `json:"operations"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	switch {
	case a.URI != "":
		return strings.TrimPrefix(a.URI, "file://")
	case a.Path != "":
		return a.Path
	case a.Root != "":
		return a.Root
	case a.Workspace != "":
		return a.Workspace
	case len(a.Paths) > 0:
		return a.Paths[0]
	case len(a.Operations) > 0:
		return a.Operations[0].Path
	}
	return ""
}
