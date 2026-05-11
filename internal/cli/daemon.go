package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/mcp"
	"github.com/golimpio/plumb/internal/memory"
	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/stats"
	"github.com/golimpio/plumb/internal/tools"
)

var daemonCmd = &cobra.Command{
	Use:    "daemon",
	Short:  "Run the background daemon (usually started automatically by serve)",
	Hidden: true,
	RunE:   runDaemon,
}

func runDaemon(_ *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
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
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
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

	slog.Info("daemon: ready", "socket", socketPath, "pid", os.Getpid())

	tools.Version = Version

	statsStore := newStatsStore()
	defer statsStore.Close()

	pool := newWorkspacePool(cfg)
	defer pool.close()

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
		wg.Add(1)
		go func() {
			defer wg.Done()
			handleConn(ctx, conn, pool, cfg, statsStore)
		}()
	}
}

// handleConn runs a complete MCP session over conn, attaching to a shared
// gopls process from pool once the workspace root is determined.
func handleConn(ctx context.Context, conn net.Conn, pool *workspacePool, cfg config.Config, statsStore *statsStore) {
	defer conn.Close()

	// Register the session immediately so it appears in `plumb sessions` and the
	// TUI as soon as the client connects — before the workspace is resolved.
	sessID, _ := session.Register(session.Info{
		DaemonVersion: Version,
		Language:      "go",
		Adapter:       "gopls",
	})
	defer session.Unregister(sessID)

	// Multi-workspace aware proxies. Each LSP tool call routes to the gopls
	// for the workspace containing its URI; diagnostics route the same way.
	// Workspace-wide methods (WorkspaceSymbols, Initialize, etc.) fall back
	// to the connection's primary workspace.
	sessionProxy := newRoutingProxy(pool)
	sessionInv := newRoutingInvProxy(pool)
	var acquiredRoot string
	var clientName, clientVersion string
	var stateMu sync.Mutex

	startGopls := func(startCtx context.Context, rootURI string) {
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

		e, err := pool.acquireLang(startCtx, folder, language)
		if err != nil {
			slog.Error("daemon: acquire LS", "root", folder, "language", language, "err", err)
			return
		}
		sessionProxy.setPrimary(folder, e.proxy)
		sessionInv.setPrimary(folder, e.inv)
		acquiredRoot = folder

		// Update the session file with the now-resolved workspace.
		cn, cv := clientName, clientVersion
		session.Patch(sessID, func(info *session.Info) {
			info.Folder = folder
			if cn != "" {
				info.ClientName = cn
				info.ClientVersion = cv
			}
		})

		slog.Info("daemon: session attached to gopls", "root", folder)
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
	srv.Register(tools.NewFindReferences(sessionProxy))
	srv.Register(tools.NewCallHierarchy(sessionProxy))
	srv.Register(tools.NewTypeHierarchy(sessionProxy))
	srv.Register(tools.NewDiagnostics(sessionInv))
	srv.Register(tools.NewListFiles())
	srv.Register(tools.NewListDirectory())
	srv.Register(tools.NewReadFile())
	srv.Register(tools.NewReadMultipleFiles())
	srv.Register(tools.NewWriteFile(sessionProxy, sessionCache, sessionInv))
	srv.Register(tools.NewEditFile(sessionProxy, sessionCache, sessionInv))
	srv.Register(tools.NewDeleteFile(sessionProxy, sessionCache))
	srv.Register(tools.NewRenameFile(sessionProxy, sessionCache))
	srv.Register(tools.NewSearchInFiles())
	srv.Register(tools.NewFindFiles())
	srv.Register(tools.NewGit())
	srv.Register(tools.NewFileDiff())
	srv.Register(tools.NewFindReplace())
	srv.Register(tools.NewVersion())
	srv.Register(tools.NewSessionStart(wsFn, sessionInv, rootsFn))

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
	}

	srv.OnAfterTool = func(_ context.Context, toolName string, args json.RawMessage, output string, dur time.Duration, isError bool) {
		// Per-project DB lives at <workspace>/.plumb/stats.db. Calls
		// without a discoverable workspace are dropped — they can't be
		// attributed to any project's history.
		stateMu.Lock()
		root := acquiredRoot
		stateMu.Unlock()
		if w := workspaceFromArgs(pool, args); w != "" {
			root = w
		}
		if root == "" {
			return
		}
		errMsg := ""
		if isError {
			errMsg = output
		}
		statsStore.Record(root, stats.Call{
			SessionID:   sessID,
			Workspace:   root,
			Tool:        toolName,
			CalledAt:    time.Now(),
			DurationMs:  dur.Milliseconds(),
			InputBytes:  len(args),
			OutputBytes: len(output),
			Success:     !isError,
			ErrorMsg:    errMsg,
		})
	}

	srv.OnInit = func(initCtx context.Context, request mcp.RequestFn) {
		requestMu.Lock()
		clientRequest = request
		requestMu.Unlock()
		rootURI := rootFromRoots(initCtx, request)
		startGopls(initCtx, rootURI)
	}

	srv.OnRootsChanged = func(initCtx context.Context, request mcp.RequestFn) {
		requestMu.Lock()
		clientRequest = request
		requestMu.Unlock()
		slog.Info("daemon: roots changed — re-fetching workspace root")
		rootURI := rootFromRoots(initCtx, request)
		startGopls(initCtx, rootURI)
	}

	srv.OnBeforeTool = func(toolCtx context.Context, _ string, args json.RawMessage) {
		stateMu.Lock()
		hasPrimary := acquiredRoot != ""
		stateMu.Unlock()
		if hasPrimary {
			return
		}
		var a struct {
			URI  string `json:"uri"`
			Path string `json:"path"`
			Root string `json:"root"` // list_files uses "root" instead of "path"
		}
		_ = json.Unmarshal(args, &a)

		// Seed the workspace lookup from URI (LSP tools) or Path/Root (filesystem tools).
		var seed string
		switch {
		case a.URI != "":
			seed = a.URI
		case a.Path != "":
			seed = "file://" + a.Path
		case a.Root != "":
			seed = "file://" + a.Root
		default:
			return
		}

		seedPath := strings.TrimPrefix(seed, "file://")
		// If seedPath is already a directory (filesystem tools pass the search
		// root, not a file), use it directly. filepath.Dir on a directory path
		// strips the last component and would miss the project root marker.
		startDir := seedPath
		if info, err := os.Stat(seedPath); err != nil || !info.IsDir() {
			startDir = filepath.Dir(seedPath)
		}
		root, _, err := pool.Detect(startDir)
		if err != nil {
			slog.Warn("daemon: cannot determine workspace root", "seed", seed, "err", err)
			return
		}
		startGopls(toolCtx, "file://"+root)
	}

	_ = srv.Serve(ctx, conn, conn)
}

// daemonSocketPath returns the Unix socket path for the plumb daemon.
func daemonSocketPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.sock")
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

// daemonLogPath returns the path where daemon logs are written.
func daemonLogPath() string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = os.TempDir()
	}
	return filepath.Join(cacheDir, "plumb", "daemon.log")
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

// findGoModRoot walks up from the directory containing fileURI (a file:// URI)
// and returns the project root using findProjectRoot.
func findGoModRoot(fileURI string) (string, error) {
	path := strings.TrimPrefix(fileURI, "file://")
	return findProjectRoot(filepath.Dir(path))
}

// findProjectRoot finds the workspace root for dir using two strategies in order:
//  1. Walk up looking for a .plumb directory — explicit project marker that takes
//     priority over nested go.mod files (e.g. in testdata or vendor).
//  2. Fall back to the nearest go.mod.
func findProjectRoot(dir string) (string, error) {
	d := dir
	for {
		if _, err := os.Stat(filepath.Join(d, ".plumb")); err == nil {
			// .plumb found; validate that a go.mod exists here or above.
			return goModRootForDir(d)
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return goModRootForDir(dir)
}

// goModRootForDir walks up from dir itself until it finds a go.mod.
func goModRootForDir(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found in or above %s", dir)
		}
		dir = parent
	}
}

// workspaceFromArgs returns the resolved workspace root for a tool call's raw
// JSON arguments, inspecting `uri` then `path`. Returns "" if neither is
// present or the path doesn't sit under a discoverable project root.
func workspaceFromArgs(pool *workspacePool, args json.RawMessage) string {
	var a struct {
		URI  string `json:"uri"`
		Path string `json:"path"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	var seed string
	switch {
	case a.URI != "":
		seed = strings.TrimPrefix(a.URI, "file://")
	case a.Path != "":
		seed = a.Path
	default:
		return ""
	}
	root, _, err := pool.Detect(filepath.Dir(seed))
	if err != nil {
		return ""
	}
	return root
}
