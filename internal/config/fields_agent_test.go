package config

import (
	"strings"
	"testing"
)

func TestRegistry_AgentAllowlist(t *testing.T) {
	writable := []string{
		"ui.theme", "ui.path_style", "log_level",
		"topology.exclude_patterns",
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
		"git.commit_trailer",
		"workspace.extra_roots",
		"workspace.read_roots",
		"workspace.auto_attach",
		"semantics.api_key",
		"session.eviction_ttl_minutes",
		"log_file",
		"lsp.go.command",
		"lsp.go.enabled",
		// quality.bin names the executable plumb runs for an analyser — the
		// [lsp.<lang>] command hole in a different table.
		"quality.bin",
		// Not a guardrail: refused because agent_config writes PROJECT scope
		// only and [quality] is read from the global store, so allowing it would
		// hand an agent a control whose every use is a silent no-op reported as
		// success.
		"quality.analysers",
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

// No [quality] key may be agent-writable. agent_config writes PROJECT scope
// only and the runner reads the global store, so any of them would be a control
// whose every use is a no-op reported as success.
//
// Stated as a rule over the whole block rather than as one entry in the deny
// list above, because a [quality] key added later inherits the property and
// would otherwise have to be remembered.
func TestRegistry_NoQualityKeyIsAgentWritable(t *testing.T) {
	for key := range agentWritableKeys {
		if strings.HasPrefix(key, "quality.") {
			t.Errorf("%q is agent-writable, but [quality] is read from the global config and "+
				"agent_config writes project scope only — the write would be a silent no-op", key)
		}
	}
	// The negative control: the rule is about [quality], not about everything.
	if !IsAgentWritable("topology.exclude_patterns") {
		t.Error("topology.exclude_patterns must stay agent-writable — it IS honoured per project")
	}
}
