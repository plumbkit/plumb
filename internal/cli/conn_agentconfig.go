package cli

// conn_agentconfig.go wires the agent_config tool to the session: it bridges the
// config-layer allowlist + atomic batch writer into the tool's plain deps, gated
// on the per-connection [agent_config_writes] knob. Mirrors the gitPolicy seam.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/tools"
)

func (s *connSession) agentConfigDeps() tools.AgentConfigDeps {
	return tools.AgentConfigDeps{
		// The connection's view answers for every agent on it: agent_config_writes
		// is ClassForcedGlobal, so no project can set it and every project's
		// resolved value is the global one.
		Enabled:  func() bool { return s.view().agentConfigWrites },
		Describe: agentDescribe,
		Apply:    s.applyAgentConfig,
	}
}

// agentDescribe renders the allowlisted writable fields for the tool's describe op.
func agentDescribe() []tools.AgentConfigField {
	fields := config.AgentWritableKeys()
	out := make([]tools.AgentConfigField, 0, len(fields))
	for _, f := range fields {
		out = append(out, tools.AgentConfigField{
			Key:           f.Key,
			Type:          f.Type.String(),
			Description:   f.Description,
			ReloadTier:    f.ReloadTier.String(),
			AllowedValues: config.EnumValues(f),
		})
	}
	return out
}

// applyAgentConfig validates + writes a batch atomically, then makes the change
// live for this connection. The allowlist is enforced here at the cli seam AND
// again inside config.AgentApplyBatch (defence in depth): a non-allowlisted key
// is refused before any disk is touched, by two independent checks.
// It writes the CALLING agent's project (#522). On a shared connection an
// agent pinned to project B that set tasks.go.lint used to rewrite the
// connection's project A, and its own run_task never saw the change.
func (s *connSession) applyAgentConfig(ctx context.Context, pairs map[string]any) (string, error) {
	ws := s.workspaceFor(ctx)
	if ws == "" {
		return "", errors.New("no workspace attached")
	}
	for k := range pairs {
		if !config.IsAgentWritable(k) {
			return "", fmt.Errorf("agent_config: %q is not an agent-writable key", k)
		}
	}
	prov := config.ProvenanceEntry{
		Source:    "agent",
		SessionID: s.sessionID(),
		Client:    s.view().clientName,
		Timestamp: time.Now(),
	}
	cfgPath := config.ProjectConfigPath(ws)
	before, _ := history.SideFromFile(cfgPath)
	changed, err := config.AgentApplyBatch(s.store.Current(), ws, pairs, prov)
	if err != nil {
		return "", err
	}
	suffix := ""
	if after, aerr := history.SideFromFile(cfgPath); aerr == nil {
		op := history.OpUpdate
		if !before.Exists {
			op = history.OpCreate
		}
		s.recordHistory(ctx, history.Change{
			At:     time.Now(),
			Op:     op,
			Kind:   history.KindFile,
			Tool:   "agent_config",
			Path:   cfgPath,
			Before: before,
			After:  after,
		})
		// The config change is a file change like any other, so it reports what it
		// did with the same policy: the .plumb/ writer had no response rendering of
		// its own, and a config file is exactly the sort of path a sensitive glob
		// names. buildWriteDeps is cheap here — this runs on an agent_config set,
		// not on a hot path.
		suffix = tools.ResponseDiffSuffix(ctx, s.buildWriteDeps(), cfgPath, before, after)
	}
	// Live before the tool returns. The connection's own project is cached in its
	// view and is re-applied; any other root is read per call (projectViewFor), so
	// the agent's next call already sees the write. Re-applying THAT root here
	// would swap another project's config into the connection's view.
	//
	// The same test is asked again inside the lane, where the swap commits: a
	// re-pin that lands between this read and the commit has applied its own root,
	// and the apply of this one must then be dropped, not laid over it (#558).
	holdsWS := func(v *sessionView) bool { return ws == v.configRoot || ws == v.acquiredRoot }
	if v := s.view(); holdsWS(&v) {
		s.applyProjectConfigIf(ws, holdsWS)
	}
	s.log().Info("daemon: agent wrote project config", "workspace", ws, "keys", changed)
	return fmt.Sprintf(
		"applied %d key(s) to %s/.plumb/config.toml (provenance=agent): %s\nrevert any with: plumb config unset <key> --workspace .",
		len(changed), ws, strings.Join(changed, ", ")) + suffix, nil
}
