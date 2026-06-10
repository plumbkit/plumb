package cli

// conn_register.go — write-tool deps assembly, MCP tool registration, and the
// MCP lifecycle-hook wiring.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tools"
)

// hasStructuralEngine reports whether path is owned by a language with a
// structural extractor (Go AST or tree-sitter, including Markdown/config), so a
// file_outline call would return a useful map. Wired into read_file to gate its
// large-read nudge. Stateless and connection-independent — a package function,
// not a method.
func hasStructuralEngine(path string) bool {
	l, ok := langsupport.ByPath(path)
	return ok && l.Structural != langsupport.EngineNone
}

// buildWriteDeps assembles the WriteDeps struct used by all write tools.
func (s *connSession) buildWriteDeps() tools.WriteDeps {
	var qualityReport tools.QualityReportFn
	if r := s.view().qualityRunner; r != nil {
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
	srv.Register(tools.NewReadFile(s.readTracker).WithBoundary(boundary).WithClient(s.clientNameStr).WithOutsideLabel(s.outsideWorkspaceLabel).WithWrites(s.writeTracker).WithOutlineHint(hasStructuralEngine))
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
	srv.Register(tools.NewWorkspaceSessions(s.workspace, s.sessID).WithBoundary(boundary))
	srv.Register(tools.NewSessionStart(s.workspace, s.sessionInv, s.rootFromClient, s.refuseHomeRoots, s.clientNameStr, s.gitPolicy).
		WithTopology(topoFn).
		WithEpisodic(s.latestEpisodic).
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
	srv.Register(tools.NewReadMemory(s.workspace).WithIndex(s.memoryIndexLive).WithBoundary(boundary).WithTopology(topoFn))
	srv.Register(tools.NewWriteMemory(s.workspace).WithIndex(s.memoryIndexLive).WithBoundary(boundary))
	srv.Register(tools.NewDeleteMemory(s.workspace).WithIndex(s.memoryIndexLive).WithBoundary(boundary))
	srv.Register(tools.NewSearchMemories(s.workspace).WithIndex(s.memoryIndexLive).WithBoundary(boundary))
	srv.Register(tools.NewRelevantMemories(s.workspace).WithBoundary(boundary))
	srv.Resources = memory.NewResourceProvider(s.workspace)
	srv.RegisterPrompt(mcp.NewOrientPrompt(s.workspace))
	srv.RegisterPrompt(mcp.NewWhatsBrokenPrompt(s.workspace))
	srv.RegisterPrompt(mcp.NewRecentChangesPrompt(s.workspace))
	srv.RegisterPrompt(mcp.NewSelftestPrompt(s.workspace))
	srv.Register(tools.NewTopologyStatus(topoFn, s.workspace).WithBoundary(boundary))
	srv.Register(tools.NewTopologySearch(topoFn).WithSemantics(s.semanticRerank))
	srv.Register(tools.NewTopologyExplore(topoFn).WithMemories(s.workspace))
	srv.Register(tools.NewTopologyImpact(topoFn))
	srv.Register(tools.NewTopologyAffected(topoFn).WithMemories(s.workspace))
	srv.Register(tools.NewTopologyRoutes(topoFn))
	srv.Register(tools.NewStructuralQuery(topoFn, s.workspace))
	srv.Register(tools.NewWorkspaceSearch(s.workspace, topoFn).WithMemoryIndex(s.memoryIndexLive))
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
	srv.EnrichToolOutput = s.enrichToolOutput
}
