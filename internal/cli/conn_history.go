package cli

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/mcp"
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
	hc := s.historyConfig()
	if !hc.Enabled {
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
