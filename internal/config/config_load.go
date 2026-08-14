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
	if d, ok := envDuration("PLUMB_GIT_WRITE_TIMEOUT"); ok {
		cfg.Git.WriteTimeout = Duration{d}
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
//
// This is also the ONE place the project-config trust boundary is decided. The
// sections that grant capability — [git] and the exec-deciding [lsp.<lang>]
// fields — are honoured only when the user has run `plumb trust` over this
// project's exact request, and forced back to base otherwise. Deciding it here
// rather than at each consumer is what makes the pool that spawns the language
// server, `plumb config show`, the TUI, the web settings API and doctor all
// correct without any of them knowing about trust.
func LoadProject(base Config, workspace string) (Config, error) {
	cfg, _, err := LoadProjectWithPolicy(base, workspace)
	return cfg, err
}

// LoadProjectWithPolicy is LoadProject plus the ProjectPolicyStatus it decided
// on, computed from the SAME bytes as the returned config.
//
// A caller that must authorise something against the config it just loaded needs
// both halves to come from one read. Re-deriving the status afterwards (via
// ProjectPolicyStatusFor, which reads the file again) opens a window in which
// the file changes between the two: a repository could be loaded with hostile
// content and then restore the file to content that IS trusted, and the check
// would pass while the loaded content is what runs. The session's exec gate
// (sessionView.execTrusted) is the caller this exists for.
func LoadProjectWithPolicy(base Config, workspace string) (Config, ProjectPolicyStatus, error) {
	merged := cloneConfig(base)
	path := ProjectConfigPath(workspace)
	data, present, err := readProjectConfigFile(path)
	if err != nil {
		return base, ProjectPolicyStatus{}, err
	}
	raw := map[string]any{}
	if present {
		if err := toml.Unmarshal(data, &merged); err != nil {
			return base, ProjectPolicyStatus{}, fmt.Errorf("parsing project config %s: %w", path, err)
		}
		if err := toml.Unmarshal(data, &raw); err != nil {
			return base, ProjectPolicyStatus{}, fmt.Errorf("parsing project config %s: %w", path, err)
		}
		merged.Git.Env = composeGitEnv(base.Git.Env, merged.Git.Env)
	}
	// The spec is computed from the same bytes that were just merged, so the
	// content trust is checked against is exactly the content in play — a second
	// read of the file could see a different version.
	st := ProjectPolicyStatus{Path: path, Spec: projectPolicySpecFrom(raw)}
	if !st.Spec.IsEmpty() {
		st.Trusted = projectPolicyTrust().IsTrustedForPolicy(workspace, st.Spec)
	}
	if !st.InEffect() {
		forceCapabilityFieldsToBase(base, &merged)
	}
	merged.LSP = dropUnknownLSPLanguages(base.LSP, merged.LSP)
	forceGlobalOnlyToBase(base, &merged)
	applyOneWayBools(base, &merged)
	// Env is the highest-priority layer, so it is applied AFTER the untrusted
	// project fields are forced back — otherwise forcing [git] to base would
	// discard a PLUMB_GIT_* override the user set for this process.
	applyEnv(&merged)
	normaliseConfig(&merged)
	if err := validate(merged); err != nil {
		return base, ProjectPolicyStatus{}, fmt.Errorf("invalid project config: %w", err)
	}
	return merged, st, nil
}

// composeGitEnv gives [git] env the composition rule every SCALAR field in this
// config already has: the project's value wins for the keys it names, and a
// global value it is silent about survives.
//
// It has to be stated explicitly because go-toml does not unmarshal the three
// spellings of a table key alike when the destination map is pre-populated (and
// merged's is — it starts as a clone of base). Under `[git]`, an inline
// `env = { X = "y" }` REPLACES the map wholesale, while a `[git.env]` sub-table
// and a `git.env.X` dotted key merge into it. Left alone, two spellings of the
// same intent would resolve to different environments for the git child, and the
// inline one would additionally let a project DELETE a variable the user set
// globally — which is precisely what the knob's own contract (it extends the
// inherited environment, and there is deliberately no way to unset an entry)
// says it cannot do on the other axis. This is a composition rule, not a
// boundary: all three spellings are trust-gated regardless.
//
// projectEnv is merged's own map (a clone of base's, or a fresh one go-toml
// built for the inline form), never base's, so filling it in place cannot reach
// the caller's config.
func composeGitEnv(baseEnv, projectEnv map[string]string) map[string]string {
	if len(baseEnv) == 0 {
		return projectEnv
	}
	out := projectEnv
	if out == nil {
		out = make(map[string]string, len(baseEnv))
	}
	for k, v := range baseEnv {
		if _, named := out[k]; !named {
			out[k] = v
		}
	}
	return out
}

// readProjectConfigFile reads a project config, reporting an absent file as
// present=false rather than an error. Returning the bytes lets LoadProject decode
// them twice — once into the Config struct, once into the raw map the trust spec
// is derived from — without reading the file twice.
func readProjectConfigFile(path string) (data []byte, present bool, err error) {
	if path == "" {
		return nil, false, nil
	}
	data, err = os.ReadFile(path)
	switch {
	case err == nil:
		return data, true, nil
	case os.IsNotExist(err):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("reading project config %s: %w", path, err)
	}
}

// forceGlobalOnlyToBase overwrites, in merged, every config section a project's
// .plumb/config.toml must not be able to set AT ALL — not even with the user's
// approval. These are daemon-global concerns (one listener, one theme) or
// user-only safety knobs whose whole purpose is that the thing they govern cannot
// widen its own permission; a per-project grant would be meaningless for the
// first group and self-defeating for the second. The sections a project MAY set
// once trusted ([git], the exec-deciding [lsp.<lang>] fields) are handled by
// forceCapabilityFieldsToBase instead.
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
	// Cloned, not aliased. These are reference types, so assigning them shares the
	// global store's backing array/map with every per-connection merged config; a
	// consumer that appended to or wrote through one would mutate the global
	// config for every other session. No current consumer does — they are all
	// read-only, and Store.Current() is itself only a shallow copy — but a forced
	// field exists precisely so a project cannot influence the global value, and
	// leaving it shared makes that a property of caller discipline rather than of
	// this function.
	merged.Workspace.ExtraRoots = slices.Clone(base.Workspace.ExtraRoots)
	merged.Workspace.ReadRoots = slices.Clone(base.Workspace.ReadRoots)
	// allow_dependency_reads widens the READ boundary to the session language's
	// toolchain roots (GOMODCACHE, the cargo registry, site-packages). It sits one
	// field below the two above and was missed when they were forced: a user who
	// set it false globally — a deliberate "do not read my module cache" — had that
	// opt-out silently reversed by any cloned repository, because buildPathPolicy
	// reads it from the per-connection merged config.
	merged.Workspace.AllowDependencyReads = base.Workspace.AllowDependencyReads
	// [tools] client_profiles narrows the advertised tool set per client, and a
	// project has no legitimate claim on which tools a given CLIENT is offered —
	// that is a property of the client, not of the repository it has open.
	merged.Tools.ClientProfiles = maps.Clone(base.Tools.ClientProfiles)
}
