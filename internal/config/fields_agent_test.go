package config

import "testing"

func TestRegistry_AgentAllowlist(t *testing.T) {
	writable := []string{
		"ui.theme", "ui.path_style", "log_level",
		"topology.exclude_patterns", "quality.analysers",
		"tasks.go.build", "tasks.python.test", "tasks.rust.verify",
	}
	for _, k := range writable {
		if !IsAgentWritable(k) {
			t.Errorf("expected %q to be agent-writable", k)
		}
	}
}

// TestRegistry_DenyListNeverWritable is the headline security test: every
// guardrail key must be refused by the single chokepoint, so a careless future
// allowlist addition that touches one of them fails here.
func TestRegistry_DenyListNeverWritable(t *testing.T) {
	denied := []string{
		"agent_config_writes", // the enable knob — never self-writable
		"edits.strict",
		"edits.rate_limit_per_minute",
		"git.allow_writes",
		"git.allow_destructive",
		"git.allow_push",
		"git.protected_branches",
		"workspace.extra_roots",
		"workspace.read_roots",
		"workspace.auto_attach",
		"semantics.api_key",
		"session.eviction_ttl_minutes",
		"log_file",
		"lsp.go.command",
		"lsp.go.enabled",
		"unknown.key",
	}
	for _, k := range denied {
		if IsAgentWritable(k) {
			t.Errorf("SECURITY: %q must NOT be agent-writable", k)
		}
	}
}

func TestAgentWritableKeys_ReturnsAllowlist(t *testing.T) {
	got := AgentWritableKeys()
	if len(got) != len(agentWritableKeys) {
		t.Errorf("AgentWritableKeys returned %d fields, want %d", len(got), len(agentWritableKeys))
	}
	for _, f := range got {
		if !agentWritableKeys[f.Key] {
			t.Errorf("AgentWritableKeys returned non-allowlisted %q", f.Key)
		}
	}
}
