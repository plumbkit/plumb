package cli

// conn_register.go — write-tool deps assembly, MCP tool registration, and the
// MCP lifecycle-hook wiring.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/toolerror"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/xcodebsp"
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
	// Resolve the quality runner lazily on each write, for the same reason as
	// topologyNotify below: buildWriteDeps runs during tool registration, before
	// the client handshake attaches the workspace and creates the runner, so an
	// eager capture here is always nil — permanently disabling the post-write
	// quality findings the [quality] feature appends to write responses.
	qualityReport := func(ctx context.Context, path string) string {
		r := s.view().qualityRunner
		if r == nil {
			return ""
		}
		return r.Report(ctx, path)
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
	// NOTE: the [edits] fsync knob is deliberately NOT installed here. It gates
	// free functions (safeWrite and friends) shared by every session, so it is
	// daemon-global: installing it per connection would make the last connection
	// to attach set the durability contract for every other session on every
	// other workspace. runDaemon installs it once from the global config store.
	return tools.WriteDeps{
		Client:                s.sessionProxy,
		Cache:                 s.sessionCache,
		Diag:                  s.sessionInv,
		Limiter:               s.writeLimiter,
		LimiterFor:            s.rateLimiterFor,
		Strict:                s.isStrict,
		Reads:                 s.readTracker,
		ReadsFor:              s.readTrackerFor,
		Writes:                s.writeTracker,
		WritesFor:             s.writeTrackerFor,
		Undo:                  s.undoStore,
		UndoFor:               s.undoStoreFor,
		PostWriteDiagWindowFn: func() time.Duration { return postWriteDiagWindow(s.editsConfig()) },
		DiagWait:              tools.NewDiagWaitEstimator(),
		CrossFileDiagFn:       func() bool { return s.editsConfig().PostWriteCrossFile },
		CrossFileSettleFn:     func() time.Duration { return crossFileSettle(s.editsConfig()) },
		ConcurrentWriteSkewFn: func() time.Duration { return concurrentWriteSkew(s.editsConfig()) },
		WorkspaceFn:           s.workspaceFor,
		Boundary:              s.writeBoundaryGuardFor,
		Contested:             s.pinContested,
		ShowWriteDiffFn:       func() bool { return s.editsConfig().ShowWriteDiff },
		BlockDirtyFn:          func() bool { return s.editsConfig().BlockDirtyWrites },
		PostWriteNotifyFn:     s.javaPostWriteNotify,
		QualityReport:         qualityReport,
		TopologyNotify:        topologyNotify,
	}
}

// onPinConflict is session_start's WithPinConflict hook: it fires only from
// tools.SessionStart.repinExplicit's no-repin-callback branch
// (internal/tools/session_start.go:565), which is the fallback used when no
// re-pin callback is wired at all — registerAllTools below always wires one
// (WithRepin(s.repinWorkspace)), so in production this fires only for a
// caller using an older/bare tools.SessionStart wiring. That branch's own
// returned error already names the correct remedy for this specific caller
// ("To switch projects, start a new MCP connection" — there is no force flag
// to retry with here, unlike the sticky-pin guard in conn_repin.go), so the
// HealthMessage recorded for the dashboard (issue #358) repeats it verbatim
// rather than leaving the operator with no next step.
func (s *connSession) onPinConflict(requested string) {
	ws := s.workspace()
	s.markBoundaryViolation(fmt.Sprintf("session_start workspace switch refused: connection is pinned to %s; requested %s. To switch projects, start a new MCP connection.", ws, requested))
}

// registerAllTools registers every MCP tool with srv.
func (s *connSession) registerAllTools(srv *mcp.Server, daemonStartedAt time.Time) {
	lspTimeout := s.store.Current().LSPQuery.Timeout.Duration
	topoFn := s.topologyStoreLive
	// Read tools (reads/searches) admit any allowed root including read-only
	// dependency roots; write/semantic-write tools demand read-write access.
	boundary := s.readBoundaryGuard
	readBoundaryFor := s.readBoundaryGuardFor
	writeBoundary := s.writeBoundaryGuardFor
	// The LSP routing proxies guard cross-workspace diagnostics queries, which
	// are reads.
	s.sessionProxy.setBoundaryGuard(boundary)
	s.sessionInv.setBoundaryGuard(boundary)
	// The warm-up probe lets the topology-fallback notes distinguish a server
	// that is still completing its handshake from one that is genuinely absent.
	warmupFn := s.sessionProxy.WarmupStatus
	xcodeHintFn := func(_ string) string {
		if s.acquiredLanguageName() != "swift" || s.workspace() == "" {
			return ""
		}
		if s.pool != nil {
			if status := s.pool.xcodeStatus(s.workspace()); status.State != "" {
				return status.Hint()
			}
		}
		return xcodebsp.Inspect(s.workspace()).Hint()
	}
	xcodeProofFn := func() {
		if s.pool != nil && s.acquiredLanguageName() == "swift" && s.workspace() != "" {
			s.pool.markXcodeSemanticProven(s.workspace())
		}
	}
	srv.Register(tools.NewWorkspaceSymbols(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout, s.workspaceFor).WithTopologyFallback(topoFn).WithLSPWarmup(warmupFn).WithXcodeHint(xcodeHintFn).WithXcodeProof(xcodeProofFn).WithContested(s.pinContested))
	srv.Register(tools.NewGetDefinition(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout).WithTopologyFallback(topoFn).WithLSPWarmup(warmupFn).WithWorkspace(s.workspaceFor).WithXcodeHint(xcodeHintFn).WithXcodeProof(xcodeProofFn).WithContested(s.pinContested))
	srv.Register(tools.NewExplainSymbol(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout).WithLSPWarmup(warmupFn).WithWorkspace(s.workspaceFor).WithContested(s.pinContested))
	srv.Register(tools.NewFileOutline(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout).WithTopologyFallback(topoFn).WithBoundary(readBoundaryFor).WithWorkspace(s.workspaceFor).WithContested(s.pinContested))
	srv.Register(tools.NewFindReferences(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout).WithLSPWarmup(warmupFn).WithWorkspace(s.workspaceFor).WithXcodeHint(xcodeHintFn).WithXcodeProof(xcodeProofFn).WithContested(s.pinContested))
	srv.Register(tools.NewCallHierarchy(s.sessionProxy, lspTimeout).WithTopologyFallback(topoFn).WithLSPWarmup(warmupFn).WithWorkspace(s.workspaceFor).WithContested(s.pinContested))
	srv.Register(tools.NewTypeHierarchy(s.sessionProxy, lspTimeout).WithLSPWarmup(warmupFn).WithWorkspace(s.workspaceFor).WithContested(s.pinContested))
	srv.Register(tools.NewDiagnosticsWithOpener(s.sessionInv, s.sessionProxy).WithBoundary(readBoundaryFor).WithLSPWarmup(warmupFn).WithWorkspace(s.workspaceFor).WithContested(s.pinContested))
	srv.Register(tools.NewReadFile(s.readTracker).WithReadsFor(s.readTrackerFor).WithBoundary(readBoundaryFor).WithClient(s.clientNameStr).WithOutsideLabel(s.outsideWorkspaceLabel).WithWrites(s.writeTracker).WithWritesFor(s.writeTrackerFor).WithOutlineHint(hasStructuralEngine).WithWorkspace(s.workspaceFor).WithContested(s.pinContested))
	srv.Register(tools.NewReadSymbol(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout, s.readTracker).WithReadsFor(s.readTrackerFor).WithTopologyFallback(topoFn).WithLSPWarmup(warmupFn).WithBoundary(readBoundaryFor).WithClient(s.clientNameStr).WithOutsideLabel(s.outsideWorkspaceLabel).WithWorkspace(s.workspaceFor).WithContested(s.pinContested))
	srv.Register(tools.NewReadMultipleFiles(s.readTracker).WithReadsFor(s.readTrackerFor).WithBoundary(readBoundaryFor).WithClient(s.clientNameStr).WithOutsideLabel(s.outsideWorkspaceLabel).WithWrites(s.writeTracker).WithWritesFor(s.writeTrackerFor).WithOutlineHint(hasStructuralEngine).WithWorkspace(s.workspaceFor).WithContested(s.pinContested))
	srv.Register(tools.NewFileStatus(s.writeTracker).WithWritesFor(s.writeTrackerFor).WithBoundary(readBoundaryFor).WithWorkspace(s.workspaceFor).WithContested(s.pinContested))
	wd := s.buildWriteDeps()
	srv.Register(tools.NewWriteFile(wd))
	srv.Register(tools.NewEditFile(wd))
	srv.Register(tools.NewDeleteFile(wd))
	srv.Register(tools.NewRenameFile(wd))
	srv.Register(tools.NewCopyFile(wd))
	srv.Register(tools.NewTransactionApply(wd))
	srv.Register(tools.NewUndoEdit(wd))
	srv.Register(tools.NewSearchInFiles(s.workspaceFor, s.sessionProxy, s.sessionCache, s.ttl).WithBoundary(readBoundaryFor).WithContested(s.pinContested))
	srv.Register(tools.NewFindFiles(s.workspaceFor).WithBoundary(readBoundaryFor).WithContested(s.pinContested))
	srv.Register(tools.NewGit(wd, s.gitPolicy).WithSession(s.sessionID, s.sessionName).
		WithPeerIntents(func() bool { return s.collabConfig().Intents }, s.collabStoreIfExists,
			func() int { return s.collabConfig().HintBudgetBytes }))
	srv.Register(tools.NewGitInit(wd))
	srv.Register(tools.NewTasks(wd, s.taskResolver))
	srv.Register(tools.NewMutationTest(wd, s.taskResolver))
	srv.Register(tools.NewRunCommand(s.commandResolver))
	srv.Register(tools.NewAgentConfig(s.agentConfigDeps()))
	srv.Register(tools.NewFileDiff().WithBoundary(readBoundaryFor).WithWorkspace(s.workspaceFor).WithContested(s.pinContested))
	srv.Register(tools.NewFindReplace(wd))
	prov := Provenance()
	srv.Register(tools.NewDaemonInfoFunc(s.sessionID, s.sessionName, Version, daemonStartedAt).
		WithSourceRevision(prov.Revision, prov.Dirty, prov.DirtyKnown).
		WithConfigStatus(func() tools.ConfigStatus {
			return tools.ConfigStatus{
				Generation:    s.store.Generation(),
				LastReloaded:  s.store.LastReloaded(),
				RestartNeeded: s.store.RestartNeeded(),
			}
		}).
		WithPurpose(s.sessionPurpose).
		WithLSPStatus(func() tools.LSPStatus {
			warming, elapsed := s.lspWarming()
			return tools.LSPStatus{
				Language:        s.acquiredLanguageName(),
				Warming:         warming,
				Elapsed:         elapsed,
				DiagnosticsMode: s.lspDiagMode(),
				Routed:          s.routedLanguageNames(),
			}
		}).
		WithToolProfile(func() (string, int, string) {
			p, reason := s.resolveToolProfile()
			if p != "lean" {
				return p, 0, reason
			}
			return p, hiddenToolCount(srv), reason
		}).
		WithPinProvenance(s.pinProvenance).
		WithProtocol(s.protocolStatus))
	srv.Register(tools.NewRenameSession(s.renameSession))
	srv.Register(tools.NewWorkspaceSessions(s.workspace, s.sessionID).WithBoundary(boundary).
		WithInheritedSessions(s.inheritedSessionIDs).
		WithTopology(topoFn).
		WithPeerAwareness(func() bool { return s.collabConfig().PeerAwareness }).
		WithCollab(
			func() (bool, bool) { c := s.collabConfig(); return c.Intents, c.Mailbox },
			s.collabStoreIfExists,
			// addressableName, NOT sessionName: an unregistered session keeps a
			// display name for the TUI and logs, but that name was drawn with no
			// uniqueness check and may shadow a live peer. Listing its mail would
			// print that peer's sender and body to the shadow.
			s.addressableName,
		).
		WithCollabObservability(
			s.collabGlobalIfExists,
			// THIS workspace's consent, not the sender's: the cross-project
			// conversation counts are rendered inside this project, so the project
			// being told that another one is talking to it is the one that has to
			// have opted in.
			func() bool { return s.collabConfig().CrossProject },
		))
	collabDeps := s.collabDeps()
	srv.Register(tools.NewShareIntent(collabDeps))
	srv.Register(tools.NewLeaveNote(collabDeps))
	srv.Register(tools.NewCheckMessages(collabDeps))
	srv.Register(tools.NewShareFindings(tools.ShareFindingsDeps{
		Workspace:           s.workspace,
		SessionID:           s.sessionID,
		Policy:              s.collabPolicy,
		Index:               s.memoryIndexLive,
		GeneratedMemoryKeep: func() int { return s.memoryConfig().GeneratedMemoryKeep },
	}))
	srv.Register(tools.NewSessionStart(s.workspaceFor, s.sessionInv, s.rootFromClient, s.refuseHomeRoots, s.clientNameStr, s.gitPolicy).
		WithTopology(topoFn).
		WithToolProfile(func() (string, int, string) {
			p, reason := s.resolveToolProfile()
			if p != "lean" {
				return p, 0, reason
			}
			return p, hiddenToolCount(srv), reason
		}).
		WithEpisodic(s.latestEpisodic).
		WithSelfSession(s.sessionID).
		WithSelfIdentity(s.sessionName).
		WithSurcharge(func() (int, int, int) {
			r := clientcaps.ProfileSurcharge(srv.ToolSchemaBytes(), srv.ToolFilter)
			return r.TotalBytes, r.Tokens, r.ToolCount
		}).
		WithCollab(func() (bool, int) {
			c := s.collabConfig()
			return c.PeerAwareness, c.HintBudgetBytes
		}).
		WithMailbox(func() (bool, tools.Inbox) {
			return s.collabConfig().Mailbox, s.inbox()
		}).
		WithLSPLanguage(s.acquiredLanguageName).
		WithLSPSkipNote(s.lspHomeSkipNote).
		WithPinProvenance(s.pinProvenance).
		WithLSPLanguages(s.acquiredLanguageLabels).
		WithLSPRouted(s.routedLanguageNames).
		WithLSPWarmup(s.lspWarming).
		WithLSPDiagMode(s.lspDiagMode).
		WithXcodeHint(xcodeHintFn).
		WithTasks(s.taskState).
		WithProjectPolicy(s.projectGitStatus).
		WithRepin(s.repinWorkspace).
		WithDeclaredAgent(s.declaredAgentCtx).
		WithPinConflict(s.onPinConflict).
		WithPurpose(s.setPurpose).
		WithExternalID(func(externalID string) string {
			session.SetExternalID(s.sessionID(), externalID)
			s.recordLogicalAgentAttach(externalID)
			// Mirror the linkage into the durable identity record. Until
			// PLAN-426 it lived only in the session JSON, which is collected
			// 24 h after the session ends — so an outage longer than that lost
			// the linkage while the identity itself survived, and
			// `plumb mail --external-id` stopped resolving a session that had
			// in fact recovered.
			s.persistIdentity()
			if prev := session.FindEnded(externalID, 24*time.Hour); prev != nil {
				// session.Rename refuses a name a live session already holds, so
				// two resumes racing on one external ID inside the grace window
				// cannot both inherit it — mailbox delivery matches on the name
				// string, and an ambiguous address silently misdelivers.
				// Resuming, not renaming: the entitlement is the external ID the
				// caller just presented, which is what lets a RESTARTED `plumb
				// serve` — new proxy secret, new session ID, same conversation —
				// take back the name its own durable record reserves.
				name, err := s.renameSessionResuming(prev.Name, externalID)
				if err == nil {
					return name
				}
				// Log it: a silently dropped inheritance looks to the caller like
				// the session_id argument did nothing at all.
				s.log().Debug("daemon: could not inherit the previous session name; keeping the generated one",
					"inherited", prev.Name, "err", err)
			}
			return ""
		}))
	showDiffFn := func() bool { return s.editsConfig().ShowWriteDiff }
	srv.Register(tools.NewRenameSymbol(s.sessionProxy, lspTimeout).WithLSPWarmup(warmupFn).WithBoundary(writeBoundary).WithWorkspace(s.workspaceFor).WithCache(s.sessionCache).WithStructuralFallback(wd).WithShowWriteDiff(showDiffFn).WithWriteDeps(wd).WithContested(s.pinContested))
	srv.Register(tools.NewInsertBeforeSymbol(s.sessionProxy, lspTimeout).WithTopologyFallback(topoFn).WithLSPWarmup(warmupFn).WithWorkspace(s.workspaceFor).WithCache(s.sessionCache).WithShowWriteDiff(showDiffFn).WithWriteDeps(wd).WithContested(s.pinContested))
	srv.Register(tools.NewInsertAfterSymbol(s.sessionProxy, lspTimeout).WithTopologyFallback(topoFn).WithLSPWarmup(warmupFn).WithWorkspace(s.workspaceFor).WithCache(s.sessionCache).WithShowWriteDiff(showDiffFn).WithWriteDeps(wd).WithContested(s.pinContested))
	srv.Register(tools.NewReplaceSymbolBody(s.sessionProxy, lspTimeout).WithTopologyFallback(topoFn).WithLSPWarmup(warmupFn).WithWorkspace(s.workspaceFor).WithCache(s.sessionCache).WithShowWriteDiff(showDiffFn).WithWriteDeps(wd).WithContested(s.pinContested))
	srv.Register(tools.NewSafeDeleteSymbol(s.sessionProxy, lspTimeout).WithLSPWarmup(warmupFn).WithWorkspace(s.workspaceFor).WithCache(s.sessionCache).WithShowWriteDiff(showDiffFn).WithWriteDeps(wd).WithContested(s.pinContested))
	srv.Register(tools.NewMoveSymbol(s.sessionProxy, lspTimeout).WithTopologyFallback(topoFn).WithLSPWarmup(warmupFn).WithWorkspace(s.workspaceFor).WithCache(s.sessionCache).WithShowWriteDiff(showDiffFn).WithWriteDeps(wd).WithContested(s.pinContested))
	srv.Register(tools.NewListMemories(s.workspaceFor).WithBoundary(readBoundaryFor))
	srv.Register(tools.NewReadMemory(s.workspaceFor).WithIndex(s.memoryIndexLive).WithBoundary(readBoundaryFor).WithTopology(topoFn))
	srv.Register(tools.NewWriteMemory(s.workspaceFor).WithIndex(s.memoryIndexLive).WithBoundary(readBoundaryFor))
	srv.Register(tools.NewDeleteMemory(s.workspaceFor).WithIndex(s.memoryIndexLive).WithBoundary(readBoundaryFor))
	srv.Register(tools.NewSearchMemories(s.workspaceFor).WithIndex(s.memoryIndexLive).WithBoundary(readBoundaryFor))
	srv.Register(tools.NewRelevantMemories(s.workspaceFor).WithBoundary(readBoundaryFor))
	srv.Resources = memory.NewResourceProvider(s.workspace)
	srv.RegisterPrompt(mcp.NewOrientPrompt(s.workspace))
	srv.RegisterPrompt(mcp.NewWhatsBrokenPrompt(s.workspace))
	srv.RegisterPrompt(mcp.NewRecentChangesPrompt(s.workspace))
	srv.RegisterPrompt(mcp.NewSelftestPrompt(s.workspace))
	srv.Register(tools.NewTopologyStatus(topoFn, s.workspace).WithBoundary(readBoundaryFor))
	srv.Register(tools.NewTopologySearch(topoFn).WithSemantics(s.semanticRerank))
	srv.Register(tools.NewTopologyExplore(topoFn).WithMemories(s.workspaceFor))
	srv.Register(tools.NewTopologyImpact(topoFn).WithCrossFileCallers(tools.NewLSPCrossFileCallers(s.sessionProxy, s.sessionCache, s.ttl, lspTimeout, s.workspace)))
	srv.Register(tools.NewTopologyAffected(topoFn).WithMemories(s.workspaceFor).WithTestScope(s.testScope))
	srv.Register(tools.NewTopologyRoutes(topoFn))
	srv.Register(tools.NewStructuralQuery(topoFn, s.workspaceFor))
	srv.Register(tools.NewWorkspaceSearch(s.workspaceFor, topoFn).WithMemoryIndex(s.memoryIndexLive))
	srv.Register(tools.NewMinimalDiffReview(topoFn).WithWorkspace(s.workspaceFor).WithBoundary(readBoundaryFor).WithContested(s.pinContested))
}

// registerHooks wires up the MCP lifecycle callbacks to connSession methods.
func (s *connSession) registerHooks(srv *mcp.Server) {
	srv.OnClientInfo = func(_ context.Context, name, version string) {
		s.onClientInfo(name, version)
	}
	srv.OnProtocolNegotiated = func(_ context.Context, offered, answered string, caps json.RawMessage) {
		s.onProtocolNegotiated(offered, answered, caps)
	}
	srv.OnAllowDirs = func(_ context.Context, dirs []string) {
		s.onAllowDirs(dirs)
	}
	srv.OnProxySession = func(_ context.Context, id string) {
		s.onProxySession(id)
	}
	srv.OnWorkspaceHint = func(_ context.Context, dir string) {
		s.onWorkspaceHint(dir)
	}
	srv.OnPinnedWorkspace = func(_ context.Context, dir string) {
		s.onPinnedWorkspace(dir)
	}
	srv.OnSessionID = func(_ context.Context, id string) {
		s.onSessionID(id)
	}
	srv.InitializeMeta = func(_ context.Context) map[string]any {
		// Runs after the param hooks above, so restoreIdentity has already
		// decided who this connection is — the identity stated here is the
		// settled one, not a guess made before recovery ran.
		return map[string]any{
			mcp.MetaSessionIdentityKey: s.identityMeta(),
			mcp.MetaDaemonInstanceKey:  daemonInstanceID(s.daemonStartedAt),
		}
	}
	srv.OnAfterTool = func(_ context.Context, toolName string, args json.RawMessage, output, errMsg string, dur time.Duration, isError bool, failure *toolerror.Error) {
		s.onAfterTool(toolName, args, output, errMsg, dur, isError, failure)
	}
	srv.OnInit = func(initCtx context.Context, request mcp.RequestFn, notify mcp.NotifyFn) {
		// Capture the notifier and seed the last-advertised profile so a later
		// profile-changing reload is detected against the seed (no spurious fire).
		s.mutate(func(v *sessionView) {
			v.notify = notify
			p, _ := s.resolveToolProfile()
			v.lastToolProfile = p
		})
		s.setClientRequest(request)
		s.attachOnInit(initCtx, request)
		s.applyProjectConfig(s.workspace())
		s.startConfigWatcher()
	}
	srv.OnRootsChanged = func(initCtx context.Context, request mcp.RequestFn) {
		s.handleRootsListChanged(initCtx, request)
	}
	srv.OnBeforeTool = func(toolCtx context.Context, name string, args json.RawMessage, logicalAgent string) {
		s.recordLogicalAgentCall(logicalAgent)
		s.onBeforeTool(toolCtx, name, args)
	}
	srv.OnToolRefusal = s.refuseSharedStateChange
	srv.EnrichToolOutput = s.enrichToolOutput
	// Echo the canonical pinned root back on a session_start(workspace=…) result,
	// so the serve proxy commits the resolved spelling as its replay pin.
	srv.ToolResultMeta = s.toolResultMeta
	// Filters tools/list to the resolved profile (lean hides commodity tools;
	// they stay callable by name). Resolved per list call, so it sees the client
	// identity set synchronously during initialize.
	srv.ToolFilter = s.toolVisible
	// Pin tools.PinnedTools into the client's context (Claude Code MCP tool
	// search) so the explicit, curated pin set — including the documented
	// discovery entry point workspace_search — is never deferred behind a
	// ToolSearch round-trip. Deliberately independent of the lean/bootstrap/
	// mailbox sets, which answer a different question (tools/list visibility);
	// see tools.PinnedTools for the set and the rationale.
	srv.AlwaysLoad = tools.IsPinned
}
