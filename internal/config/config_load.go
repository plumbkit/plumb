package config

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/plumbkit/plumb/internal/paths"
)

// GlobalConfigPath returns the path where the global config file lives.
// Useful for diagnostics that want to report where settings come from.
func GlobalConfigPath() string {
	return configPath()
}

// CacheDir returns the path to the ephemeral plumb cache directory.
// This is for disposable state: sockets, pids, locks.
func CacheDir() string {
	return cachePath()
}

// DataDir returns the path to the persistent plumb data directory.
// This is for important history: stats.db, telemetry.
func DataDir() string {
	return dataPath()
}

// configPath resolves the global config file via internal/paths (adrg/xdg), with
// a read fallback to the pre-0.9.8 location so existing installs are not reset.
func configPath() string {
	newPath := filepath.Join(paths.ConfigDir(), "config.toml")
	// Back-compat: plumb < 0.9.8 used the Linux XDG layout on every OS
	// (~/.config/plumb) for config. On macOS the lib-resolved location is now
	// ~/Library/Application Support/plumb; if it has no config yet but a legacy
	// file exists, keep reading the legacy one rather than silently resetting to
	// defaults. On Linux the two paths coincide, so this is a no-op there.
	if legacy := legacyConfigPath(); legacy != "" && legacy != newPath && !fileExists(newPath) && fileExists(legacy) {
		return legacy
	}
	return newPath
}

// legacyConfigPath is the pre-0.9.8 config location (XDG_CONFIG_HOME or
// ~/.config), retained only as a read fallback for existing installs.
func legacyConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "plumb", "config.toml")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func cachePath() string {
	return paths.CacheDir()
}

func dataPath() string {
	return paths.DataDir()
}

// Load reads the config file, applies env overrides, and validates the result.
// A missing config file is not an error — defaults are returned.
func Load() (Config, error) {
	cfg := cloneConfig(defaults)

	path := configPath()
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if err := toml.Unmarshal(data, &cfg); err != nil {
				return Config{}, fmt.Errorf("parsing config %s: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return Config{}, fmt.Errorf("reading config %s: %w", path, err)
		}
	}

	applyEnv(&cfg)
	normaliseConfig(&cfg)

	if err := validate(cfg); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// applyEnv overlays environment variables onto cfg.
func applyEnv(cfg *Config) {
	if v := os.Getenv("PLUMB_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("PLUMB_LOG_FILE"); v != "" {
		cfg.LogFile = v
	}
	if v := os.Getenv("PLUMB_LOG_FORMAT"); v != "" {
		cfg.LogFormat = v
	}
	applyEditsEnv(cfg)
	applyGitEnv(cfg)
	if v, ok := envBool("PLUMB_REFUSE_HOME_ROOTS"); ok {
		cfg.Walk.RefuseHomeRoots = v
	}
	if v, ok := envBool("PLUMB_AUTO_ATTACH"); ok {
		cfg.Workspace.AutoAttach = v
	}
	if v, ok := envBool("PLUMB_AUTO_ATTACH_PERSIST"); ok {
		cfg.Workspace.AutoAttachPersist = v
	}
	if d, ok := envDuration("PLUMB_LSP_QUERY_TIMEOUT"); ok {
		cfg.LSPQuery.Timeout = Duration{d}
	}
	if v, ok := envBool("PLUMB_XCODE_AUTO_BUILD_SERVER"); ok {
		cfg.Xcode.AutoBuildServer = v
	}
	if v := os.Getenv("PLUMB_TOOLS_PROFILE"); v != "" {
		cfg.Tools.Profile = v
	}
	if v, ok := envBool("PLUMB_PERSIST_SESSION_STATE"); ok {
		cfg.Session.PersistState = v
	}
	normaliseConfig(cfg)
}

// applyEditsEnv overlays the [edits] environment variables onto cfg.
func applyEditsEnv(cfg *Config) {
	if v, ok := envBool("PLUMB_STRICT_EDITS"); ok {
		cfg.Edits.Strict = v
	}
	if n, ok := envNonNegInt("PLUMB_WRITE_RATE_LIMIT"); ok {
		cfg.Edits.RateLimitPerMinute = n
	}
	if n, ok := envNonNegInt("PLUMB_POST_WRITE_DIAG_MS"); ok {
		cfg.Edits.PostWriteDiagnosticsMs = n
	}
	if n, ok := envNonNegInt("PLUMB_CONCURRENT_WRITE_SKEW_MS"); ok {
		cfg.Edits.ConcurrentWriteSkewMs = n
	}
	if v, ok := envBoolNeg("PLUMB_SHOW_WRITE_DIFF"); ok {
		cfg.Edits.ShowWriteDiff = v
	}
	if v, ok := envBoolNeg("PLUMB_BLOCK_DIRTY_WRITES"); ok {
		cfg.Edits.BlockDirtyWrites = v
	}
	if v, ok := envBoolNeg("PLUMB_FSYNC"); ok {
		cfg.Edits.Fsync = v
	}
	if v, ok := envBoolNeg("PLUMB_POST_WRITE_CROSS_FILE"); ok {
		cfg.Edits.PostWriteCrossFile = v
	}
	if n, ok := envNonNegInt("PLUMB_POST_WRITE_CROSS_FILE_SETTLE_MS"); ok {
		cfg.Edits.PostWriteCrossFileSettleMs = n
	}
}

// applyGitEnv overlays the [git] environment variables onto cfg.
func applyGitEnv(cfg *Config) {
	if v, ok := envBoolNeg("PLUMB_GIT_ALLOW_WRITES"); ok {
		cfg.Git.AllowWrites = v
	}
	if v, ok := envBool("PLUMB_GIT_ALLOW_DESTRUCTIVE"); ok {
		cfg.Git.AllowDestructive = v
	}
	if v, ok := envBool("PLUMB_GIT_ALLOW_PUSH"); ok {
		cfg.Git.AllowPush = v
	}
	if v, ok := envBool("PLUMB_GIT_COMMIT_TRAILER"); ok {
		cfg.Git.CommitTrailer = v
	}
}

// envBool reads key from the environment. ok is true when the variable is
// set; v reflects whether the value is a recognised truthy string.
func envBool(key string) (v bool, ok bool) {
	s := os.Getenv(key)
	if s == "" {
		return false, false
	}
	return s == "1" || s == "true" || s == "yes", true
}

// envBoolNeg reads key from the environment and returns the logical inverse
// of recognised falsy strings ("0", "false", "no"). ok is true when set.
func envBoolNeg(key string) (v bool, ok bool) {
	s := os.Getenv(key)
	if s == "" {
		return false, false
	}
	return s != "0" && s != "false" && s != "no", true
}

// envNonNegInt reads key from the environment and parses it as a
// non-negative integer. ok is false when unset, unparseable, or negative.
func envNonNegInt(key string) (n int, ok bool) {
	s := os.Getenv(key)
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// envDuration reads key from the environment and parses it as a non-negative
// Go duration (e.g. "30s", "2m"). ok is false when unset, unparseable, or
// negative.
func envDuration(key string) (d time.Duration, ok bool) {
	s := os.Getenv(key)
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}

func normaliseConfig(cfg *Config) {
	if cfg.Workspace.AutoAttachPersist {
		cfg.Workspace.AutoAttach = true
	}
	for name, lsp := range cfg.LSP {
		if expanded := expandPath(lsp.Command); expanded != lsp.Command {
			lsp.Command = expanded
			cfg.LSP[name] = lsp
		}
	}
}

// expandPath expands environment variables and a leading "~"/"~/" in an LSP
// command path so config files stay portable across machines. Thin wrapper over
// paths.ExpandHome (the shared implementation).
func expandPath(s string) string {
	return paths.ExpandHome(s)
}

// ProjectConfigPath returns the conventional location of a workspace's
// plumb-local config: <workspace>/.plumb/config.toml.
func ProjectConfigPath(workspace string) string {
	if workspace == "" {
		return ""
	}
	return filepath.Join(workspace, ".plumb", "config.toml")
}

// LoadProject reads <workspace>/.plumb/config.toml and merges it onto base.
// Missing file is not an error; base is returned unchanged. Environment
// variable overrides are re-applied so they remain the highest-priority
// layer. Validation is performed after the merge.
//
// Call this once per connection, after the workspace has been resolved.
// The result is what tools should consult for per-project settings (strict
// mode, rate limit).
func LoadProject(base Config, workspace string) (Config, error) {
	merged := cloneConfig(base)
	path := ProjectConfigPath(workspace)
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := toml.Unmarshal(data, &merged); err != nil {
				return base, fmt.Errorf("parsing project config %s: %w", path, err)
			}
		case os.IsNotExist(err):
			// no project config — fall through, env still applied
		default:
			return base, fmt.Errorf("reading project config %s: %w", path, err)
		}
	}
	forceGlobalOnlyToBase(base, &merged)
	// Env is the highest-priority layer, so it is applied AFTER the untrusted
	// project fields are forced back — otherwise forcing [git] to base would
	// discard a PLUMB_GIT_* override the user set for this process.
	applyEnv(&merged)
	normaliseConfig(&merged)
	if err := validate(merged); err != nil {
		return base, fmt.Errorf("invalid project config: %w", err)
	}
	return merged, nil
}

// forceGlobalOnlyToBase overwrites, in merged, every config section a project's
// .plumb/config.toml must not be able to set. A project config is an UNTRUSTED
// surface — cloning a repository ships it, and it takes effect on attach with no
// prompt — so any setting that grants a capability rather than expressing a
// preference is global-only, and the trusted global value is forced back here.
// One chokepoint keeps the guarantee true for every consumer (the daemon gates,
// `config show`, the TUI display, the web settings API).
func forceGlobalOnlyToBase(base Config, merged *Config) {
	// agent_config_writes is a user-only, global-scope safety knob. A project's
	// .plumb/config.toml is an untrusted surface (a cloned repo ships it), so it
	// must never be able to ENABLE the agent-writable-config tool — otherwise the
	// "the agent can never widen its own permission" invariant breaks the moment a
	// user opens a hostile repo. Force the global value to win regardless of what
	// the project file sets. This single chokepoint keeps the guarantee true for
	// every consumer (the daemon gate, `config show`, the TUI display).
	merged.AgentConfigWrites = base.AgentConfigWrites
	// [web] and [ui] are daemon-global presentation settings, not per-project
	// concerns — the web server is one process-wide listener and the theme is a
	// single global preference. Force the global values to win so a project's
	// (untrusted) .plumb/config.toml cannot rebind the web port or flip the theme.
	merged.Web = base.Web
	merged.UI = base.UI
	// [semantics] configures an OUTBOUND embedding endpoint plus the credentials
	// sent to it. Like [web]/[ui] it is a daemon-global concern, not a per-project
	// one. If an untrusted project .plumb/config.toml could set provider/base_url/
	// api_key_env, a routine topology_search would become an SSRF + secret-exfil
	// primitive: Resolve() runs os.Getenv on the attacker-named variable and the
	// client POSTs it as a bearer token to the attacker-named URL. Force the global
	// (trusted) values to win regardless of what the project file sets.
	merged.Semantics = base.Semantics
	// [workspace] extra_roots / read_roots widen the filesystem-access allowlist —
	// extra_roots to read-WRITE roots, read_roots to read-only — and take effect on
	// attach with no per-call confirmation. A cloned hostile repo shipping
	// extra_roots = ["/"] would otherwise gain read-write access outside the
	// workspace the moment a session attaches, defeating the "a write outside the
	// workspace is refused by construction" invariant. These roots are global-only
	// (the agent_config tool already treats them as un-writable); force them to base.
	merged.Workspace.ExtraRoots = base.Workspace.ExtraRoots
	merged.Workspace.ReadRoots = base.Workspace.ReadRoots
	// [git] is the git tool's tiered safety policy in its entirety — the gate that
	// decides whether the destructive tier (reset, clean, checkout, rebase) and the
	// network tier (push, fetch, pull) may run at all, and which branches can never
	// be force-pushed. The connection builds its live tools.GitPolicy straight from
	// this block, with no per-call trust check anywhere behind it. A cloned hostile
	// repo shipping allow_destructive = true, allow_push = true and an empty
	// protected_branches would therefore grant itself history destruction and
	// arbitrary pushes to the user's remotes, using the user's credentials, the
	// moment a session attaches. Every field here is a safety decision the user
	// makes about a project, never one the project makes about itself; force the
	// whole block to base.
	merged.Git = base.Git
	// [lsp.<lang>] command/args/env are the argv and environment of a process the
	// daemon spawns (pool → lsp.NewSupervisor → exec.CommandContext), so a project
	// config that could set them is arbitrary code execution: cloning a repo whose
	// .plumb/config.toml says `command = "/bin/sh"` would run the attacker's
	// command as the user, unsandboxed, on attach, with no `plumb trust` and no
	// agent involved. Force those — and the two fields one hop behind them — back
	// to the trusted global config; see forceLSPExecToBase for the field-by-field
	// reasoning and for what a project may still override.
	merged.LSP = forceLSPExecToBase(base.LSP, merged.LSP)
}

// forceLSPExecToBase returns merged's per-language [lsp.<lang>] tables with every
// field that decides WHICH process runs, or WITH WHAT, taken from base (the
// trusted global config) rather than from the project's untrusted file:
//
//   - command, args, env — the literal argv and environment of the spawned
//     language server (exec.CommandContext in lsp.Supervisor.spawn). Setting env
//     is execution too, not merely configuration: PATH re-points which binary a
//     server invokes, and DYLD_INSERT_LIBRARIES / LD_PRELOAD inject code into it.
//   - initialization_options — passed verbatim to the server as LSP
//     initializationOptions, which for real servers is a command channel:
//     rust-analyzer's check.overrideCommand runs an arbitrary argv, and zls's
//     enable_build_on_save runs the project's own build.zig on every save.
//   - root_markers, weak_root_markers — workspace detection, i.e. which language
//     server is elected for a directory. Rebinding them cannot invent a binary,
//     but it does choose one from the installed set (electing java to get jdtls
//     onto a repo's build.gradle, say), so it belongs with the rest.
//
// Everything else in the table stays project-overridable, because none of it can
// change the process or its inputs: diagnostics (push/pull protocol negotiation),
// enabled (a project switching a language off, or on against the user's own
// trusted command), and idle_timeout / max_workspaces (hibernation and eviction
// budgets). A language present in the project file but absent from base is
// dropped entirely — there is no trusted command to fall back to, and plumb has
// no adapter for a language its global config does not define.
//
// Values are cloned so the returned config never shares backing storage with
// base (LoadProject's caller usually holds base in a live config store).
func forceLSPExecToBase(base, merged map[string]LSPConfig) map[string]LSPConfig {
	if merged == nil {
		return nil
	}
	out := make(map[string]LSPConfig, len(merged))
	for name, lspCfg := range merged {
		trusted, ok := base[name]
		if !ok {
			continue
		}
		lspCfg.Command = trusted.Command
		lspCfg.Args = slices.Clone(trusted.Args)
		lspCfg.Env = maps.Clone(trusted.Env)
		lspCfg.InitializationOptions = maps.Clone(trusted.InitializationOptions)
		lspCfg.RootMarkers = slices.Clone(trusted.RootMarkers)
		lspCfg.WeakRootMarkers = slices.Clone(trusted.WeakRootMarkers)
		out[name] = lspCfg
	}
	return out
}
