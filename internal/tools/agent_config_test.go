package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func agentExec(t *testing.T, tool *AgentConfig, args string) (string, error) {
	t.Helper()
	return tool.Execute(context.Background(), json.RawMessage(args))
}

func describeDeps(enabled bool) AgentConfigDeps {
	return AgentConfigDeps{
		Enabled:  func() bool { return enabled },
		Describe: func() []AgentConfigField { return []AgentConfigField{{Key: "log_level", Type: "enum"}} },
		Apply:    func(context.Context, map[string]any) (string, error) { return "applied", nil },
	}
}

func TestAgentConfig_DescribeAlwaysWorks(t *testing.T) {
	tool := NewAgentConfig(describeDeps(false))
	out, err := agentExec(t, tool, `{"op":"describe"}`)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !strings.Contains(out, "disabled") || !strings.Contains(out, "log_level") {
		t.Errorf("describe output: %s", out)
	}
}

func TestAgentConfig_SetRefusedWhenDisabled(t *testing.T) {
	tool := NewAgentConfig(describeDeps(false))
	if _, err := agentExec(t, tool, `{"op":"set","set":{"log_level":"warn"}}`); err == nil {
		t.Error("set must be refused when the enable knob is off")
	}
}

func TestAgentConfig_SetAppliesWhenEnabled(t *testing.T) {
	tool := NewAgentConfig(describeDeps(true))
	out, err := agentExec(t, tool, `{"op":"set","set":{"log_level":"warn"}}`)
	if err != nil || out != "applied" {
		t.Errorf("set: out=%q err=%v", out, err)
	}
}

func TestAgentConfig_GlobalScopeRefused(t *testing.T) {
	tool := NewAgentConfig(describeDeps(true))
	if _, err := agentExec(t, tool, `{"op":"set","set":{"log_level":"warn"},"scope":"global"}`); err == nil {
		t.Error("global scope must be refused in v1")
	}
}

// TestAgentConfig_SelfEscalationRejectedByApply proves the tool has no path to
// widen the allowlist: a deny-listed key reaches Apply (the daemon's
// AgentApplyBatch), which refuses it. The tool itself never decides writability.
func TestAgentConfig_SelfEscalationRejectedByApply(t *testing.T) {
	deps := describeDeps(true)
	deps.Apply = func(_ context.Context, pairs map[string]any) (string, error) {
		for k := range pairs {
			if k == "edits.strict" || k == "agent_config_writes" {
				return "", fmt.Errorf("%q is not an agent-writable key", k)
			}
		}
		return "applied", nil
	}
	tool := NewAgentConfig(deps)
	for _, attack := range []string{
		`{"op":"set","set":{"edits.strict":false}}`,
		`{"op":"set","set":{"agent_config_writes":true}}`,
	} {
		if _, err := agentExec(t, tool, attack); err == nil {
			t.Errorf("self-escalation must fail: %s", attack)
		}
	}
}
