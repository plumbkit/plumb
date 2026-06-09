package cli

// conn_subsystems.go — per-session topology + quality subsystems, the Java
// post-write notification, and tool-call stats recording.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/quality"
	"github.com/plumbkit/plumb/internal/quality/golangcilint"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/topology"
)

// startTopologyIndexer acquires the topology store for the workspace when
// topology is enabled, writing it into the snapshot being built. Call only from
// within a mutate fn (it reads and writes v). No-op if already started.
func (s *connSession) startTopologyIndexer(v *sessionView, workspace string) {
	if v.topologyStore != nil {
		return
	}
	if s.topologyPool == nil {
		return
	}
	cfg := s.topologyConfigFor(workspace)
	if !cfg.Enabled {
		return
	}
	v.topologyStore = s.topologyPool.Acquire(workspace, cfg)
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
// is disabled or the workspace has not yet attached. It reads the snapshot so it
// reflects a store attached after tool registration: registerAllTools — which
// builds the write-tool deps and the topology accessor — runs before the client
// handshake attaches the workspace.
func (s *connSession) topologyStoreLive() *topology.Store {
	return s.view().topologyStore
}

// reconcileTopologyStore refreshes the session's topology store after a global
// config reload. The daemon-level subscriber (notified first — see config.Store's
// registration-order guarantee) runs topoPool.Reconcile, which may have closed or
// re-opened the pooled store for this root, leaving s.topologyStore on a closed
// handle; and a live enable/disable changes whether a store should exist at all.
// Re-acquiring (or clearing) here keeps the session's topology tools on a live
// store, so enabling/disabling topology takes effect on the current session, not
// only the next one. The project-config read happens before the mutation lane is
// entered.
func (s *connSession) reconcileTopologyStore(workspace string) {
	if s.topologyPool == nil {
		return
	}
	cfg := s.topologyConfigFor(workspace)
	s.mutate(func(v *sessionView) {
		if !cfg.Enabled {
			v.topologyStore = nil
			return
		}
		v.topologyStore = s.topologyPool.Acquire(workspace, cfg)
	})
}

// memoryConfigFor returns the merged per-project [memory] config for workspace,
// so a per-project enable/disable wins over the global default.
func (s *connSession) memoryConfigFor(workspace string) config.MemoryConfig {
	base := s.store.Current()
	cfg, err := config.LoadProject(base, workspace)
	if err != nil {
		return base.Memory
	}
	return cfg.Memory
}

// memoryIndexLive returns the FTS index for the connection's current workspace,
// or nil when memory indexing is disabled, no workspace is attached, or the
// index cannot be opened. The pool opens the index lazily on first call.
func (s *connSession) memoryIndexLive() *memory.Index {
	if s.memoryPool == nil {
		return nil
	}
	ws := s.view().acquiredRoot
	if ws == "" || !s.memoryConfigFor(ws).Enabled {
		return nil
	}
	return s.memoryPool.Acquire(ws)
}

// startQualityRunner creates and starts the quality runner when the [quality]
// block is enabled, writing it into the snapshot being built. Call only from
// within a mutate fn (it reads and writes v). No-op if already started.
func (s *connSession) startQualityRunner(v *sessionView, workspace string) {
	if v.qualityRunner != nil {
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
	v.qualityRunner = r
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

// javaPostWriteNotify sends DidOpen + DidClose to jdtls after a write so that
// it publishes fresh diagnostics. No-op for non-Java workspaces.
func (s *connSession) javaPostWriteNotify(ctx context.Context, path string) error {
	lang := s.view().acquiredLanguage
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

// onAfterTool records a completed tool call in the stats store and refreshes
// the session's last-seen timestamp so idle detection stays accurate.
func (s *connSession) onAfterTool(toolName string, args json.RawMessage, output, errMsg string, dur time.Duration, isError bool) {
	session.Touch(s.sessID)
	v := s.view()
	root := v.acquiredRoot
	sessionName := v.sessName
	clientName := v.clientName
	clientVersion := v.clientVersion
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
