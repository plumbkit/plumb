package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// omp (oh-my-pi) keeps every user-level resource — MCP servers, skills,
// extensions — under one agent directory, normally ~/.omp/agent. Its location
// moves with three environment variables, resolved here in omp's own order
// (omp docs: config-usage.md "Profiles", environment-variables.md):
//
//   - OMP_PROFILE, or PI_PROFILE when OMP_PROFILE is undefined, selects a named
//     profile, whose agent directory is <root>/profiles/<name>/agent. An empty,
//     whitespace or "default" value selects the default profile.
//   - PI_CODING_AGENT_DIR replaces the default profile's agent directory
//     outright; named profiles ignore it.
//   - PI_CONFIG_DIR renames the root directory under home (default ".omp").
//
// Registration writes the primary user file, <agent dir>/mcp.json, in the plain
// mcpServers shape (the Claude Code entry, with an explicit "type": "stdio"
// the bundled schema expects). omp also imports ~/.claude.json, but its native
// file outranks every imported source, so this entry is the one omp uses.

// ompTarget is named, unlike the other extraSetupTargets rows, because the omp
// hooks pack (hooks_omp.go) depends on it as its registration.
var ompTarget = setupTarget{
	use: "omp", name: "omp", pathFn: OmpConfigPath, installedFn: ompInstalled,
	intoFn: setupClaudeCodeInto, extractFn: claudeDesktopCommandExtractor, outFn: removeMcpServersJSON,
	note: ompSetupNote, skillsDirFn: ompSkillsDir,
}

// ompAgentDir returns omp's active user agent directory. It does not check
// whether the directory exists.
func ompAgentDir() (string, error) {
	profile, set := os.LookupEnv("OMP_PROFILE")
	if !set {
		profile = os.Getenv("PI_PROFILE")
	}
	profile = strings.TrimSpace(profile)
	named := profile != "" && profile != "default"

	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" && !named {
		return dir, nil
	}
	root := ".omp"
	if dir := os.Getenv("PI_CONFIG_DIR"); dir != "" {
		root = dir
	}
	if named {
		return homeRelConfigPath(root, "profiles", profile, "agent")
	}
	return homeRelConfigPath(root, "agent")
}

// OmpConfigPath returns omp's user MCP config (<agent dir>/mcp.json).
func OmpConfigPath() (string, error) {
	dir, err := ompAgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mcp.json"), nil
}

// ompInstalled reports whether omp looks installed: its agent directory exists.
// omp creates it on first run, but mcp.json only appears once a server is
// configured, so the bulk paths and doctor need the directory to tell
// "installed, no MCP servers yet" from "not installed".
func ompInstalled() bool {
	dir, err := ompAgentDir()
	return err == nil && dirExists(dir)
}

// ompSetupNote reminds the user that a running omp session picks the change up
// without a restart.
func ompSetupNote() string {
	return "omp — run /mcp reload in a running session to pick up the change without restarting."
}
