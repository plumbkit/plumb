package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// agent_config.go is the agent-writable-config MCP tool. It has two ops:
// describe (always available — lists the allowlisted keys the agent MAY write)
// and set (gated on the per-connection enable knob, writes a batch atomically).
// The security model lives below this layer: the enable knob and the writable
// allowlist are enforced in the daemon-supplied deps (and re-enforced in
// config.AgentApplyBatch), so no code path here can widen what is writable —
// the agent can only propose key/value pairs. No config import (layering); the
// daemon bridges config through AgentConfigDeps.
//
// Concurrency: Execute is safe for concurrent use (no shared mutable state).

// AgentConfigField is one writable key's metadata, surfaced by describe.
type AgentConfigField struct {
	Key           string
	Type          string
	Description   string
	ReloadTier    string
	AllowedValues []string
}

// AgentConfigDeps is the seam the daemon fills (mirrors GitPolicyFn /
// TaskResolverFn). All fields are required for a functioning tool; nil Enabled
// is treated as disabled.
type AgentConfigDeps struct {
	// Enabled reports whether agent config writes are turned on for this
	// connection (the user-only [agent_config_writes] knob).
	Enabled func() bool
	// Describe returns the allowlisted writable fields.
	Describe func() []AgentConfigField
	// Apply validates and writes a batch atomically, hot-reloads the connection,
	// and stamps provenance. It returns a human-readable result, or an error
	// (including for any non-allowlisted key — defence in depth).
	Apply func(ctx context.Context, pairs map[string]any) (string, error)
}

// AgentConfig is the agent_config MCP tool.
type AgentConfig struct {
	deps AgentConfigDeps
}

func NewAgentConfig(deps AgentConfigDeps) *AgentConfig {
	return &AgentConfig{deps: deps}
}

var agentConfigSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "op": {
      "type": "string",
      "enum": ["describe", "set"],
      "description": "describe: list the config keys you are allowed to write (always available). set: write a batch of key/value pairs (only when the user has enabled [agent_config_writes])."
    },
    "set": {
      "type": "object",
      "description": "For op=set: a map of dotted config key to value, e.g. {\"tasks.go.test\": \"go test ./...\", \"log_level\": \"warn\"}. Validated and applied atomically (all-or-nothing) to the project config; a key outside the allowlist is refused."
    },
    "scope": {
      "type": "string",
      "enum": ["project"],
      "description": "Write scope. Only \"project\" (the workspace's .plumb/config.toml) is supported; global writes are out of scope."
    }
  },
  "required": ["op"],
  "additionalProperties": false
}`)

func (t *AgentConfig) Name() string                 { return "agent_config" }
func (t *AgentConfig) InputSchema() json.RawMessage { return agentConfigSchema }
func (t *AgentConfig) Description() string {
	return "Read and (when enabled) write a small allowlist of plumb config keys on the user's behalf — task commands ([tasks.<lang>]), log level, theme, topology excludes, quality analysers. " +
		"op=describe lists exactly what you may write (always available); op=set writes a batch to the project's .plumb/config.toml, validated and applied all-or-nothing, tagged provenance=agent and one-step revertible (plumb config unset). " +
		"Writing is OFF unless the user enabled [agent_config_writes]; safety-critical keys (git tiers, workspace roots, strict mode, API keys, the enable knob itself) are never writable. Use it to set up a repo's build/test commands from what you can read in the project."
}

type agentConfigArgs struct {
	Op    string         `json:"op"`
	Set   map[string]any `json:"set"`
	Scope string         `json:"scope"`
}

func (t *AgentConfig) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a agentConfigArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("agent_config: invalid arguments: %w", err)
	}
	switch a.Op {
	case "describe":
		return t.describe(), nil
	case "set":
		return t.set(ctx, a)
	default:
		return "", fmt.Errorf("agent_config: op must be \"describe\" or \"set\"; got %q", a.Op)
	}
}

func (t *AgentConfig) enabled() bool {
	return t.deps.Enabled != nil && t.deps.Enabled()
}

func (t *AgentConfig) describe() string {
	var b strings.Builder
	if t.enabled() {
		b.WriteString("agent config writes: ENABLED (op=set will write to the project config)\n")
	} else {
		b.WriteString("agent config writes: disabled — the user must set [agent_config_writes] = true to allow op=set\n")
	}
	var fields []AgentConfigField
	if t.deps.Describe != nil {
		fields = t.deps.Describe()
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Key < fields[j].Key })
	fmt.Fprintf(&b, "writable keys (%d):\n", len(fields))
	for _, f := range fields {
		fmt.Fprintf(&b, "  %s (%s", f.Key, f.Type)
		if f.ReloadTier != "" {
			fmt.Fprintf(&b, ", %s", f.ReloadTier)
		}
		b.WriteString(")")
		if len(f.AllowedValues) > 0 {
			fmt.Fprintf(&b, " one of [%s]", strings.Join(f.AllowedValues, ", "))
		}
		fmt.Fprintf(&b, "\n    %s\n", f.Description)
	}
	return b.String()
}

func (t *AgentConfig) set(ctx context.Context, a agentConfigArgs) (string, error) {
	if a.Scope != "" && a.Scope != "project" {
		return "", fmt.Errorf("agent_config: only scope=\"project\" is supported (global writes are out of scope)")
	}
	if len(a.Set) == 0 {
		return "", fmt.Errorf("agent_config: set requires a non-empty \"set\" map of key/value pairs")
	}
	if !t.enabled() {
		return "", fmt.Errorf("agent_config: writes are disabled; ask the user to set [agent_config_writes] = true (they alone can enable it)")
	}
	if t.deps.Apply == nil {
		return "", fmt.Errorf("agent_config: writing is not available for this session")
	}
	return t.deps.Apply(ctx, a.Set)
}
