package config

// fields_agent.go is the single security chokepoint for the agent-writable-config
// tool: agentWritableKeys lists exactly the config keys the agent may write. A
// key absent here is never agent-writable (fail closed). The deliberately
// EXCLUDED keys are the guardrails — git tiers (allow_destructive/allow_push/
// protected_branches), workspace roots (extra_roots/read_roots/auto_attach),
// edits.strict and edits.rate_limit_per_minute, semantics.api_key, session
// eviction, log_file, lsp.* server config, quality.bin (it names an executable),
// and agent_config_writes itself (so the agent can never widen its own
// permission). Keeping the list in one small file makes the security surface
// auditable at a glance.
//
// One key was removed rather than excluded for safety: quality.analysers. The
// agent_config tool writes at PROJECT scope only, and [quality] is read from the
// global store (see QualityConfig), so allowing it handed an agent a control
// whose every use was a silent no-op — config written, never read, reported as
// success. A control that writes TOML plumb ignores is worse than no control.
//
// KNOWN, AND DELIBERATELY NOT CHANGED HERE: ui.theme, ui.path_style and
// log_level have the same property — config.AppliesAtProjectScope answers false
// for all three — so an agent's project-scope write of them is equally inert.
// They are left alone because the remedy is not obviously withdrawal: a
// per-project log level is a coherent thing to want, and making it APPLY would
// serve the agent better than refusing it. Deciding that is a change to what
// agent_config means, not to what [quality] does, and it is not this change's to
// make.

// agentWritableKeys is the allowlist, keyed by registry (template) key. Only
// ergonomic, non-guardrail settings appear here.
var agentWritableKeys = map[string]bool{
	"ui.theme":                  true,
	"ui.path_style":             true,
	"log_level":                 true,
	"topology.exclude_patterns": true,
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
