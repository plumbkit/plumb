package config

// fields_agent.go is the single security chokepoint for the agent-writable-config
// tool: agentWritableKeys lists exactly the config keys the agent may write. A
// key absent here is never agent-writable (fail closed). The deliberately
// EXCLUDED keys are the guardrails — git tiers (allow_destructive/allow_push/
// protected_branches), workspace roots (extra_roots/read_roots/auto_attach),
// edits.strict and edits.rate_limit_per_minute, semantics.api_key, session
// eviction, log_file, lsp.* server config, and agent_config_writes itself (so
// the agent can never widen its own permission). Keeping the list in one small
// file makes the security surface auditable at a glance.

// agentWritableKeys is the allowlist, keyed by registry (template) key. Only
// ergonomic, non-guardrail settings appear here.
var agentWritableKeys = map[string]bool{
	"ui.theme":                  true,
	"ui.path_style":             true,
	"log_level":                 true,
	"topology.exclude_patterns": true,
	"quality.analysers":         true,
	"tasks.<lang>.build":        true,
	"tasks.<lang>.lint":         true,
	"tasks.<lang>.test":         true,
	"tasks.<lang>.e2e":          true,
	"tasks.<lang>.verify":       true,
}

// IsAgentWritable reports whether the agent-writable-config tool may write the
// given concrete dotted key. Per-language family keys are normalised to their
// template before the allowlist check; an unknown key is never writable.
func IsAgentWritable(key string) bool {
	if _, ok := Lookup(key); !ok {
		return false
	}
	return agentWritableKeys[normaliseFamilyKey(key)]
}

// AgentWritableKeys returns the registry fields the agent is permitted to write
// (templates for per-language families). Used by the tool's describe op.
func AgentWritableKeys() []Field {
	out := make([]Field, 0, len(agentWritableKeys))
	for _, f := range Registry() {
		if agentWritableKeys[f.Key] {
			out = append(out, f)
		}
	}
	return out
}
