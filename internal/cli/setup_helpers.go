package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"

	"github.com/plumbkit/plumb/internal/fsync"
)

// claudeCodeConfigPath returns the user-level Claude Code config path.
func claudeCodeConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// homeRelConfigPath joins parts under the user's home directory. It is the
// common shape of the per-client config-path helpers below.
func homeRelConfigPath(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, parts...)...), nil
}

// CursorConfigPath returns the global Cursor MCP config (~/.cursor/mcp.json),
// shared by the Cursor editor and the cursor-agent CLI.
func CursorConfigPath() (string, error) {
	return homeRelConfigPath(".cursor", "mcp.json")
}

// AugmentConfigPath returns the Augment Code (auggie CLI) settings path
// (~/.augment/settings.json).
func AugmentConfigPath() (string, error) {
	return homeRelConfigPath(".augment", "settings.json")
}

// QwenConfigPath returns the Qwen Code settings path (~/.qwen/settings.json).
// Qwen Code is a Gemini-CLI fork and shares its mcpServers JSON shape.
func QwenConfigPath() (string, error) {
	return homeRelConfigPath(".qwen", "settings.json")
}

// KimiCodeConfigPath returns the Kimi Code MCP config path (~/.kimi-code/mcp.json),
// the plain mcpServers JSON shape Kimi Code shares with Claude Desktop. The
// legacy Kimi Desktop read the same file, but Kimi Work (the daimon-based
// desktop app) does NOT — it reads the mcp.json inside its own bundled kernel
// home, so it takes a separate target (KimiWorkConfigPath). KIMI_CODE_HOME
// overrides the config directory, mirroring Codex's CODEX_HOME.
func KimiCodeConfigPath() (string, error) {
	if home := os.Getenv("KIMI_CODE_HOME"); home != "" {
		return filepath.Join(home, "mcp.json"), nil
	}
	return homeRelConfigPath(".kimi-code", "mcp.json")
}

// kimiCodeInstalled reports whether Kimi Code looks installed: its data dir
// ($KIMI_CODE_HOME, or ~/.kimi-code when unset) exists. Kimi Code creates that
// dir on first run, but mcp.json only appears once an MCP server is configured,
// so config-file presence alone cannot tell "installed, no MCP servers yet"
// from "not installed" — the bulk setup paths and doctor use this to tell them
// apart.
func kimiCodeInstalled() bool {
	if home := os.Getenv("KIMI_CODE_HOME"); home != "" {
		return dirExists(home)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return dirExists(filepath.Join(home, ".kimi-code"))
}

// kimiWorkKernelHome returns the kimi-code kernel home the Kimi Work desktop
// app bundles inside its own data dir
// (~/Library/Application Support/kimi-desktop/daimon-share/daimon/runtime/kimi-code/home
// on macOS — verified against the live app, 2026-08). The app spawns its agent
// kernel with this as home, so this — not ~/.kimi-code — is where its mcp.json
// lives and why the kimi-code target can never register it. KIMI_WORK_HOME
// overrides the home, mirroring KIMI_CODE_HOME (and giving tests a temp dir).
// Non-macOS platforms get an explicit error: the app's data layout there is
// unverified, so plumb names the override rather than guessing a path.
func kimiWorkKernelHome() (string, error) {
	if home := os.Getenv("KIMI_WORK_HOME"); home != "" {
		return home, nil
	}
	if runtime.GOOS != "darwin" {
		return "", errors.New("the Kimi Work data layout is verified on macOS only — set KIMI_WORK_HOME to the app's kernel home to register it on this platform")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "kimi-desktop",
		"daimon-share", "daimon", "runtime", "kimi-code", "home"), nil
}

// KimiWorkConfigPath returns the Kimi Work desktop app's MCP config
// (<kernel home>/mcp.json), the plain mcpServers JSON shape the app shares
// with the CLI. It does not check whether the file exists.
func KimiWorkConfigPath() (string, error) {
	home, err := kimiWorkKernelHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "mcp.json"), nil
}

// kimiWorkInstalled reports whether Kimi Work looks installed: its bundled
// kernel home exists. The app creates that tree on first run, but mcp.json
// only appears once an MCP server is configured — the same absent-file
// ambiguity as Kimi Code — so the bulk paths and doctor use the dir to tell
// "installed, nothing configured yet" from "not installed".
func kimiWorkInstalled() bool {
	home, err := kimiWorkKernelHome()
	return err == nil && dirExists(home)
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // G703: stats a config dir the user themself points at ($KIMI_CODE_HOME) or ~/.kimi-code, as the invoking user — no privilege boundary crossed and no file contents read
	return err == nil && info.IsDir()
}

// OpenCodeConfigPath returns the OpenCode global config
// (~/.config/opencode/opencode.json).
func OpenCodeConfigPath() (string, error) {
	return homeRelConfigPath(".config", "opencode", "opencode.json")
}

// CrushConfigPath returns the Crush global config (~/.config/crush/crush.json).
func CrushConfigPath() (string, error) {
	return homeRelConfigPath(".config", "crush", "crush.json")
}

// GooseConfigPath returns the Goose config (~/.config/goose/config.yaml).
func GooseConfigPath() (string, error) {
	return homeRelConfigPath(".config", "goose", "config.yaml")
}

// HermesConfigPath returns the Hermes Agent config (~/.hermes/config.yaml).
func HermesConfigPath() (string, error) {
	return homeRelConfigPath(".hermes", "config.yaml")
}

// ZCodeConfigPath returns the ZCode user config (~/.zcode/cli/config.json), the
// file both the desktop client and its agent core read for MCP servers (under
// the nested mcp.servers key — see setup_zcode.go).
func ZCodeConfigPath() (string, error) {
	return homeRelConfigPath(".zcode", "cli", "config.json")
}

// zcodeInstalled reports whether ZCode looks installed: its home dir (~/.zcode)
// exists. The desktop app creates the dir on first run, but cli/config.json only
// appears once a setting is configured there — the same absent-file ambiguity
// that gives Kimi Code an installedFn — so the bulk paths and doctor use the dir
// to tell "installed, nothing configured yet" from "not installed".
func zcodeInstalled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return dirExists(filepath.Join(home, ".zcode"))
}

// JunieConfigPath returns the Junie user config (~/.junie/mcp/mcp.json).
func JunieConfigPath() (string, error) {
	return homeRelConfigPath(".junie", "mcp", "mcp.json")
}

// junieInstalled reports whether Junie looks installed: its home dir (~/.junie)
// exists.
func junieInstalled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return dirExists(filepath.Join(home, ".junie"))
}

// AntigravityConfigPath returns the global Antigravity CLI MCP config
// (~/.gemini/antigravity-cli/mcp/plumb.json).
func AntigravityConfigPath() (string, error) {
	return homeRelConfigPath(".gemini", "antigravity-cli", "mcp", "plumb.json")
}

// AntigravityDesktopConfigPath returns the global Antigravity Desktop MCP config
// (~/.gemini/antigravity/mcp/plumb.json).
func AntigravityDesktopConfigPath() (string, error) {
	return homeRelConfigPath(".gemini", "antigravity", "mcp", "plumb.json")
}

// GeminiConfigPath returns the platform-specific path for Gemini CLI's
// settings.json. It does not check whether the file exists.
func GeminiConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "settings.json"), nil
}

// CodexConfigPath returns the Codex config.toml path. CODEX_HOME overrides the
// default home-relative config directory.
func CodexConfigPath() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// backupFile copies src to src.<timestamp>.bak in the same directory.
func backupFile(src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-150405")
	dst := src + "." + stamp + ".bak"
	return os.WriteFile(dst, data, 0o600) //nolint:gosec // G703: dst is derived from OS-native config path helpers (UserHomeDir, Executable), not user input
}

// claudeDesktopConfigBaseDir returns the platform-specific directory Claude
// Desktop stores its per-user data under: macOS's Application Support,
// Windows's %APPDATA%, or the unofficial Linux ~/.config. Shared by
// claudeDesktopConfigPath (the one path Anthropic documents) and
// claudeDesktopExtraConfigPaths (the heuristic sibling-profile scan below).
func claudeDesktopConfigBaseDir() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA environment variable not set")
		}
		return appData, nil
	default:
		// Unofficial Linux path — Claude Desktop isn't fully supported there yet.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config"), nil
	}
}

// claudeDesktopConfigPath returns the platform-specific path for
// claude_desktop_config.json. It does not check whether the file exists.
// This is the ONE location Anthropic's own docs name (support.claude.com) —
// there is no officially documented multi-profile config path.
func claudeDesktopConfigPath() (string, error) {
	base, err := claudeDesktopConfigBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Claude", "claude_desktop_config.json"), nil
}

// claudeDesktopProfileGlob is NOT an Anthropic-documented mechanism. It is a
// best-effort match against a well-known *unofficial* community convention for
// running multiple Claude Desktop accounts on one machine — launching with
// Electron's --user-data-dir="$HOME/Library/Application Support/Claude-<name>"
// flag, or installing the .app bundle a second time under a different name
// (macOS then gives it its own Application Support directory automatically).
// Because it's a naming heuristic rather than a spec, it can both over-match (an
// unrelated "Claude Extras" folder that happens to hold a config-shaped file)
// and under-match (a profile not named with a literal "Claude" prefix).
const claudeDesktopProfileGlob = "Claude*"

// claudeDesktopExtraConfigPaths heuristically discovers sibling Claude Desktop
// profile configs alongside the canonical one — see claudeDesktopProfileGlob.
// Only directories that already contain a claude_desktop_config.json are
// returned, so plumb never materialises a new profile out of thin air; the
// canonical path itself is excluded from the result.
func claudeDesktopExtraConfigPaths() ([]string, error) {
	base, err := claudeDesktopConfigBaseDir()
	if err != nil {
		return nil, err
	}
	canonical, err := claudeDesktopConfigPath()
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(base, claudeDesktopProfileGlob, "claude_desktop_config.json"))
	if err != nil {
		return nil, nil // malformed pattern is unreachable here — degrade to "no extras found"
	}
	sort.Strings(matches)
	extras := make([]string, 0, len(matches))
	for _, m := range matches {
		if m != canonical {
			extras = append(extras, m)
		}
	}
	return extras, nil
}

// claudeDesktopConfigPaths returns the canonical Claude Desktop config path
// (always first, always included even if it doesn't exist yet, so setup still
// creates it on a first run) plus any heuristically-discovered sibling profiles
// (claudeDesktopExtraConfigPaths).
func claudeDesktopConfigPaths() ([]string, error) {
	canonical, err := claudeDesktopConfigPath()
	if err != nil {
		return nil, err
	}
	extras, err := claudeDesktopExtraConfigPaths()
	if err != nil {
		return nil, err
	}
	return append([]string{canonical}, extras...), nil
}

// readOrInitClaudeConfig reads cfgPath as JSON into a generic map.
// isNew is true when the file did not exist (empty map returned for first run).
// Any read or parse error is returned — never silently discarded.
func readOrInitClaudeConfig(path string) (m map[string]any, isNew bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, false, fmt.Errorf("creating directory: %w", err)
		}
		return map[string]any{}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return map[string]any{}, false, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("parsing %s as JSON: %w — will not overwrite", path, err)
	}
	return m, false, nil
}

// readOrInitCodexConfig reads cfgPath as TOML into a generic map.
// isNew is true when the file did not exist.
func readOrInitCodexConfig(path string) (m map[string]any, isNew bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, false, fmt.Errorf("creating directory: %w", err)
		}
		return map[string]any{}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return map[string]any{}, false, nil
	}
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("parsing %s as TOML: %w — will not overwrite", path, err)
	}
	return m, false, nil
}

// parseJSONConfig reads cfgPath as JSON, creating NOTHING — not the file, not
// its parent directory. It is the inspection-only counterpart to
// readOrInitClaudeConfig, for `plumb doctor` checks: a check must never write to
// the filesystem it is reporting on.
func parseJSONConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s as JSON: %w", path, err)
	}
	return m, nil
}

// parseTOMLConfig is parseJSONConfig for Codex's TOML config.
func parseTOMLConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s as TOML: %w", path, err)
	}
	return m, nil
}

// setupWriteOptions is the write policy for every file `plumb setup` touches.
//
// These land in OTHER tools' directories — ~/.codex, the Claude skills dir, a
// Gemini config tree — whose scanners plumb does not control, so the staging
// file is always dot-prefixed. Mode matters here too: os.CreateTemp makes 0600
// and these writers never chmod'd, so the first time `plumb setup` rewrote a
// user's existing 0644 config it silently downgraded it. AtomicWrite preserves
// an existing file's mode; 0600 applies only to one plumb creates itself.
func setupWriteOptions(tempPattern string) fsync.Options {
	return fsync.Options{TempPattern: tempPattern, Label: "setup"}
}

// writeJSON writes m to path as indented JSON, creating the file if needed.
// It writes to a temp file in the same directory and renames atomically.
func writeJSON(path string, m map[string]any) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	return fsync.AtomicWrite(path, data, setupWriteOptions(".plumb_setup_*.json"))
}

// writeTOML writes m to path as TOML, creating the file if needed.
// It writes to a temp file in the same directory and renames atomically.
func writeTOML(path string, m map[string]any) error {
	data, err := toml.Marshal(m)
	if err != nil {
		return err
	}

	return fsync.AtomicWrite(path, data, setupWriteOptions(".plumb_setup_*.toml"))
}

// readOrInitYAMLConfig reads cfgPath as YAML into a generic map.
// isNew is true when the file did not exist.
func readOrInitYAMLConfig(path string) (m map[string]any, isNew bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, false, fmt.Errorf("creating directory: %w", err)
		}
		return map[string]any{}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return map[string]any{}, false, nil
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("parsing %s as YAML: %w — will not overwrite", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, false, nil
}

// writeYAML writes m to path as YAML, creating the file if needed.
// It writes to a temp file in the same directory and renames atomically.
func writeYAML(path string, m map[string]any) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return err
	}

	return fsync.AtomicWrite(path, data, setupWriteOptions(".plumb_setup_*.yaml"))
}

// removeKey is the entry VALUE meaning "delete this key from the existing
// server entry". mergeServerEntry merges rather than replaces, so deletion is
// the one edit its vocabulary otherwise lacks — needed by
// `plumb setup <client>` (no --lean), which must be able to take a client-side
// tool allowlist back off. It never reaches a serialiser: the merge loop deletes
// the key instead of assigning the sentinel.
//
// That holds at the TOP level of a server entry, which is the only place any
// caller puts one. A sentinel NESTED inside a value would be marshalled like any
// other empty struct — `{}` in JSON, and in TOML an empty table HEADER
// (`[mcp_servers.plumb.enabled_tools]`, no braces at all) — producing a silently
// malformed config rather than an error. Nothing constructs one, and the writer
// tests decode every config they produce and reject an empty group anywhere in
// it (assertNoSentinelOnDisk) so it stays that way.
type removeKey struct{}

// mergeServerEntry is the shared, format-agnostic merge used by every
// `plumb setup <client>` command. It reads cfgPath via read, finds (or creates)
// the server map under serversKey, and inserts entry under the "plumb" key —
// preserving every other entry. read/write select the serialisation (JSON, TOML,
// or YAML); same reports whether an existing plumb entry already points at this
// binary, making the operation idempotent.
//
// Returns added=false (no write) when plumb is already registered identically.
// preserved lists the names of the other servers that were kept.
func mergeServerEntry(
	cfgPath, serversKey string,
	read func(string) (map[string]any, bool, error),
	write func(string, map[string]any) error,
	entry map[string]any,
	same func(existing map[string]any) bool,
) (added bool, preserved []string, err error) {
	cfg, isNew, err := read(cfgPath)
	if err != nil {
		return false, nil, fmt.Errorf("reading %s: %w", cfgPath, err)
	}

	if cfg[serversKey] == nil {
		cfg[serversKey] = map[string]any{}
	}
	servers, ok := cfg[serversKey].(map[string]any)
	if !ok {
		return false, nil, fmt.Errorf("%s in %s is not an object — cannot safely modify it", serversKey, cfgPath)
	}

	for name := range servers {
		if name != "plumb" {
			preserved = append(preserved, name)
		}
	}
	sort.Strings(preserved)

	existing, _ := servers["plumb"].(map[string]any)
	if existing != nil && same(existing) {
		return false, preserved, nil
	}

	if !isNew {
		if err := backupFile(cfgPath); err != nil {
			return false, nil, fmt.Errorf("backing up %s: %w", cfgPath, err)
		}
	}

	// Merge the canonical fields onto any existing plumb entry rather than
	// replacing it wholesale, so user-added keys survive a re-register or a
	// `plumb setup --all` repoint — most importantly Codex's per-tool
	// [mcp_servers.plumb.tools.*] approval tables, which a replace would drop.
	if existing == nil {
		existing = map[string]any{}
	}
	for k, v := range entry {
		if _, drop := v.(removeKey); drop {
			delete(existing, k)
			continue
		}
		existing[k] = v
	}
	servers["plumb"] = existing

	if err := write(cfgPath, cfg); err != nil {
		return false, nil, fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return true, preserved, nil
}

// The skills-directory resolvers and installSkill live in setup_skills.go, the
// one home for the skill-delivery seam.

func stringSliceEqual(got any, want []string) bool {
	gotSlice, ok := got.([]any)
	if !ok {
		return false
	}
	if len(gotSlice) != len(want) {
		return false
	}
	for i, gotItem := range gotSlice {
		if gotItem != want[i] { //nolint:gosec // G602: len(gotSlice)==len(want) is asserted by the length guard above
			return false
		}
	}
	return true
}
