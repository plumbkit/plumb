package cli

import (
	"fmt"
	"sort"
)

// ZCode (Z.ai's desktop client) is the one JSON target whose servers live one
// level deeper than a serversKey: mcp.servers.<name> in ~/.zcode/cli/config.json,
// a file that also carries the client's hooks and plugin state. Its server
// schema is strict — ZCode's own bundled diagnosing-mcp guide states an unknown
// key causes the server to be dropped, and the desktop Settings → MCP page
// crashes on an argv-array command (`command.trim is not a function`) — so the
// entry is exactly {type, command, args}, with command a plain string and type
// spelled out (the desktop reader prefers canonical fields over legacy
// migration). There is no --lean here for the same reason: a client-side
// allowlist key would make ZCode drop the plumb server entirely.

// setupZCodeInto registers plumb in ZCode's user config under the nested
// mcp.servers key, preserving sibling servers and the file's other resources.
// It keeps mergeServerEntry's contract (idempotent, backed-up before the first
// change, preserved server names) but cannot reuse it: the servers map sits one
// level under "mcp", which the shared helper's single serversKey cannot
// express.
func setupZCodeInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	cfg, isNew, err := readOrInitClaudeConfig(cfgPath)
	if err != nil {
		return false, nil, fmt.Errorf("reading %s: %w", cfgPath, err)
	}

	servers, err := zcodeServersMap(cfg, cfgPath)
	if err != nil {
		return false, nil, err
	}

	for name := range servers {
		if name != "plumb" {
			preserved = append(preserved, name)
		}
	}
	sort.Strings(preserved)

	existing, _ := servers["plumb"].(map[string]any)
	if existing != nil && existing["command"] == plumbBin {
		return false, preserved, nil
	}

	if !isNew {
		if err := backupFile(cfgPath); err != nil {
			return false, nil, fmt.Errorf("backing up %s: %w", cfgPath, err)
		}
	}
	if existing == nil {
		existing = map[string]any{}
	}
	// Merge the canonical fields onto any existing entry — a user's own
	// timeoutMs or env survives a repoint, the contract every other client's
	// re-registration keeps.
	existing["type"] = "stdio"
	existing["command"] = plumbBin
	existing["args"] = []string{"serve"}
	servers["plumb"] = existing

	if err := writeJSON(cfgPath, cfg); err != nil {
		return false, nil, fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return true, preserved, nil
}

// zcodeServersMap returns the mcp.servers map from cfg, creating the mcp and
// servers objects when absent. A non-object at either level is an error — the
// same refusal mergeServerEntry gives a non-object serversKey — because
// overwriting it would take the user's hooks and plugin state with it.
func zcodeServersMap(cfg map[string]any, cfgPath string) (map[string]any, error) {
	mcpAny, ok := cfg["mcp"]
	if !ok || mcpAny == nil {
		mcpAny = map[string]any{}
		cfg["mcp"] = mcpAny
	}
	mcp, ok := mcpAny.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mcp in %s is not an object — cannot safely modify it", cfgPath)
	}
	serversAny, ok := mcp["servers"]
	if !ok || serversAny == nil {
		serversAny = map[string]any{}
		mcp["servers"] = serversAny
	}
	servers, ok := serversAny.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mcp.servers in %s is not an object — cannot safely modify it", cfgPath)
	}
	return servers, nil
}

// zcodeCommandExtractor reads the launch binary back from ZCode's nested
// mcp.servers.plumb.command — one level deeper than mapCommandExtractor's
// single serversKey reaches, so it descends "mcp" itself and reuses
// registeredCommand for the inner map.
func zcodeCommandExtractor(cfgPath string) (binPath string, registered bool, err error) {
	cfg, _, err := readOrInitClaudeConfig(cfgPath)
	if err != nil {
		return "", false, err
	}
	mcp, _ := cfg["mcp"].(map[string]any)
	if mcp == nil {
		return "", false, nil
	}
	bin, ok := registeredCommand(mcp, "servers", "command")
	return bin, ok, nil
}
