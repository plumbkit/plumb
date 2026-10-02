package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools/txlog"
)

// recordHistory is WriteDeps.HistoryFn: it stamps the connection's identity,
// the call id and the attributed workspace onto c, classifies it inline
// (history.Prepare — sensitive content never enters the queue) and enqueues.
// Workspace attribution reuses onAfterTool's resolution so a call's history
// rows and its stats row are filed under the same project.
func (s *connSession) recordHistory(ctx context.Context, c history.Change) {
	if s == nil || s.historyStore == nil {
		return
	}
	agent := mcp.LogicalAgentFromCtx(ctx)
	v := s.view()
	root := v.acquiredRoot
	if w := s.recordedRootFor(agent); w != "" {
		root = w
	}
	if w := workspaceFromArgs(s.pool, pathArg(c.Path), root); w != "" {
		root = w
	}
	if root == "" {
		root = filepath.Dir(c.Path)
	}
	hc := s.historyConfigFor(root)
	if !hc.Enabled {
		return
	}
	callID := mcp.CallIDFromCtx(ctx)
	if callID == "" {
		callID = "nocall-" + mcp.NewCallID()
	}
	it := history.Item{
		Change:       c,
		CallID:       callID,
		SessionID:    s.sessionID(),
		SessionName:  v.sessName,
		LogicalAgent: s.attributedAgent(agent),
		ClientName:   v.clientName,
		Workspace:    root,
	}
	s.historyStore.Enqueue(history.Prepare(it, history.Policy{
		SensitiveGlobs:  hc.SensitiveGlobs,
		MaxContentBytes: hc.MaxContentBytes,
	}))
}

// pathArg shapes a path as the tool-argument JSON workspaceFromArgs reads.
func pathArg(p string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"file_path": p})
	return b
}

// txlogRecoverySink records txlog crash-recovery restores as reverts
// (reason crash_recovery). A manifest from before call_id linking gets a
// synthetic recovery-<ulid> call id; its reverts_seq stays NULL.
func (s *connSession) txlogRecoverySink(_ string) txlog.RestoreSink {
	return func(r txlog.Restored) {
		callID := r.CallID
		if callID == "" {
			callID = "recovery-" + mcp.NewCallID()
		}
		ctx := mcp.WithCallID(context.Background(), callID)
		s.recordHistory(ctx, history.Change{
			At:             time.Now(),
			Op:             history.OpRevert,
			Kind:           history.KindFile,
			Tool:           "txlog_recovery",
			Path:           r.Path,
			Before:         r.Before,
			After:          history.SideFromBytes(r.After),
			RevertsOwnCall: r.CallID != "",
			Reason:         "crash_recovery",
		})
	}
}

func (s *connSession) historyConfig() config.HistoryConfig {
	return s.view().history
}

// historyConfigFor is the [history] config of the project a write LANDED in.
// The connection's view describes only the project it was loaded for, so a write
// into another root — an agent pinned elsewhere on a shared connection, or an
// absolute path into a sibling project — would otherwise be classified by the
// wrong project's sensitive_globs (#522 is the same split for run_task). The
// common case, a write into the loaded project, reads the view untouched; any
// other root is loaded from disk, and an unreadable project config falls back to
// the GLOBAL config, never to the connection's project.
func (s *connSession) historyConfigFor(root string) config.HistoryConfig {
	v := s.view()
	if root == "" || root == v.configRoot || (v.configRoot == "" && root == v.acquiredRoot) {
		return v.history
	}
	if s.store == nil {
		return v.history
	}
	cfg, _, err := config.LoadProjectWithPolicy(s.store.Current(), root)
	if err != nil {
		s.log().Warn("daemon: project config invalid for a history write; using global", "workspace", root, "err", err)
		return s.store.Current().History
	}
	return cfg.History
}
