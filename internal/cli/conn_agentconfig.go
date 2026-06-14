package cli

// conn_agentconfig.go wires the agent_config tool to the session: it bridges the
// config-layer allowlist + atomic batch writer into the tool's plain deps, gated
// on the per-connection [agent_config_writes] knob. Mirrors the gitPolicy seam.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

func (s *connSession) agentConfigDeps() tools.AgentConfigDeps {
	return tools.AgentConfigDeps{
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

// applyAgentConfig validates + writes a batch atomically (config.AgentApplyBatch
// re-enforces the allowlist), then makes the change live for this connection.
func (s *connSession) applyAgentConfig(_ context.Context, pairs map[string]any) (string, error) {
	ws := s.workspace()
	if ws == "" {
		return "", fmt.Errorf("no workspace attached")
	}
	prov := config.ProvenanceEntry{
		Source:    "agent",
		SessionID: s.sessID,
		Client:    s.view().clientName,
		Timestamp: time.Now(),
	}
	changed, err := config.AgentApplyBatch(s.store.Current(), ws, pairs, prov)
	if err != nil {
		return "", err
	}
	s.applyProjectConfig(ws) // live for this connection before the tool returns
	s.log().Info("daemon: agent wrote project config", "workspace", ws, "keys", changed)
	return fmt.Sprintf(
		"applied %d key(s) to %s/.plumb/config.toml (provenance=agent): %s\nrevert any with: plumb config unset <key> --workspace .",
		len(changed), ws, strings.Join(changed, ", ")), nil
}
