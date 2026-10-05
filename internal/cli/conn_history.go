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
	root := s.historyRootFor(ctx, c.Path)
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
		// The same verdict the response gate takes, so the two cannot disagree.
		Sensitive: s.changeSensitive(ctx, c.Path, c.From),
	}))
}

// historyOnFor reports whether recordHistory would record a write to path, so
// a write site outside WriteDeps can skip reading its sides when it would not.
func (s *connSession) historyOnFor(ctx context.Context, path string) bool {
	return s != nil && s.historyStore != nil && s.historyConfigFor(s.historyRootFor(ctx, path)).Enabled
}

// historyRootFor is the project a write to path is attributed to: the calling
// agent's root, then the project the path itself lies in (onAfterTool's
// resolution, so history and stats file a call under the same project), else
// the path's directory.
func (s *connSession) historyRootFor(ctx context.Context, path string) string {
	root := s.view().acquiredRoot
	if w := s.recordedRootFor(mcp.LogicalAgentFromCtx(ctx)); w != "" {
		root = w
	}
	// No pool (a session not yet attached to the daemon's, as tests build them)
	// has no project to resolve the path against; the agent's root stands.
	if s.pool != nil {
		if w := workspaceFromArgs(s.pool, pathArg(path), root); w != "" {
			root = w
		}
	}
	if root == "" {
		root = filepath.Dir(path)
	}
	return root
}

// changeSensitive is the ONE decision about whether a change's content may be
// seen. It is WriteDeps.SensitivePathFn (the tool result, the copy that leaves
// the machine) and recordHistory's Policy.Sensitive (history.db), so the two
// agree by construction.
//
// A change with a source spans two projects when a copy or rename crosses them,
// so it is sensitive when EITHER project's globs say so: the destination
// project's globs about either path (what IsSensitiveChange asks), or the
// source project's globs about the source. Asking each path only of its own
// project, or both only of the destination's, leaks in one direction or the
// other when a glob is declared in only one of them.
func (s *connSession) changeSensitive(ctx context.Context, path, from string) bool {
	root := s.historyRootFor(ctx, path)
	if history.IsSensitiveChange(s.historyConfigFor(root).SensitiveGlobs, root, path, from) {
		return true
	}
	if from == "" {
		return false
	}
	srcRoot := s.historyRootFor(ctx, from)
	return srcRoot != root &&
		history.IsSensitiveChange(s.historyConfigFor(srcRoot).SensitiveGlobs, srcRoot, from, "")
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
