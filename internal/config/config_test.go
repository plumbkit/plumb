package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandPath(t *testing.T) {
	t.Setenv("HOME", "/test/home")
	t.Setenv("MYDIR", "/my/dir")

	cases := []struct {
		in   string
		want string
	}{
		{"~/go/bin/gopls", "/test/home/go/bin/gopls"},
		{"~", "/test/home"},
		{"$HOME/go/bin/gopls", "/test/home/go/bin/gopls"},
		{"$MYDIR/gopls", "/my/dir/gopls"},
		{"/absolute/path/gopls", "/absolute/path/gopls"},
		{"gopls", "gopls"},
		{"", ""},
	}
	for _, tc := range cases {
		got := expandPath(tc.in)
		if got != tc.want {
			t.Errorf("expandPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormaliseConfig_LSPCommandExpanded(t *testing.T) {
	t.Setenv("HOME", "/test/home")
	cfg := defaults
	cfg.LSP = map[string]LSPConfig{
		"go": {Command: "~/go/bin/gopls", Enabled: true},
		"py": {Command: "$HOME/.local/bin/pyright-langserver", Enabled: true},
		"rs": {Command: "rust-analyzer", Enabled: true},
	}
	normaliseConfig(&cfg)
	if got := cfg.LSP["go"].Command; got != "/test/home/go/bin/gopls" {
		t.Errorf("go command = %q, want /test/home/go/bin/gopls", got)
	}
	if got := cfg.LSP["py"].Command; got != "/test/home/.local/bin/pyright-langserver" {
		t.Errorf("py command = %q, want /test/home/.local/bin/pyright-langserver", got)
	}
	if got := cfg.LSP["rs"].Command; got != "rust-analyzer" {
		t.Errorf("rs command = %q, want rust-analyzer (bare name unchanged)", got)
	}
}

func TestLoadProject_MissingFile_NoError(t *testing.T) {
	base := defaults
	got, err := LoadProject(base, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Edits.Strict != base.Edits.Strict {
		t.Errorf("Strict = %v, want %v (base)", got.Edits.Strict, base.Edits.Strict)
	}
}

func TestLoadProject_OverridesEdits(t *testing.T) {
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `[edits]
strict = true
rate_limit_per_minute = 30
`
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadProject(defaults, ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Edits.Strict {
		t.Error("Strict should be true after project override")
	}
	if got.Edits.RateLimitPerMinute != 30 {
		t.Errorf("RateLimitPerMinute = %d, want 30", got.Edits.RateLimitPerMinute)
	}
	// Unrelated fields preserved from base.
	if got.LogLevel != defaults.LogLevel {
		t.Errorf("LogLevel = %q, want preserved %q", got.LogLevel, defaults.LogLevel)
	}
}

func TestLoadProject_OverridesGit(t *testing.T) {
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Project sets only allow_destructive; the other git fields must inherit.
	cfg := "[git]\nallow_destructive = true\n"
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadProject(defaults, ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Git.AllowDestructive {
		t.Error("AllowDestructive should be true after project override")
	}
	// Unset git fields preserved from base defaults.
	if !got.Git.AllowWrites {
		t.Error("AllowWrites should remain true (default) when not set by project")
	}
	if got.Git.AllowPush {
		t.Error("AllowPush should remain false (default) when not set by project")
	}
	if len(got.Git.ProtectedBranches) != 2 || got.Git.ProtectedBranches[0] != "main" {
		t.Errorf("ProtectedBranches should remain default, got %v", got.Git.ProtectedBranches)
	}
}

func TestLoadProject_GitEnvOverridesProject(t *testing.T) {
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	_ = os.MkdirAll(plumbDir, 0o755)
	_ = os.WriteFile(filepath.Join(plumbDir, "config.toml"),
		[]byte("[git]\nallow_writes = true\nallow_push = true\n"), 0o644)

	t.Setenv("PLUMB_GIT_ALLOW_WRITES", "0")
	t.Setenv("PLUMB_GIT_ALLOW_PUSH", "false")

	got, err := LoadProject(defaults, ws)
	if err != nil {
		t.Fatal(err)
	}
	if got.Git.AllowWrites {
		t.Error("env should have forced AllowWrites to false")
	}
	if got.Git.AllowPush {
		t.Error("env should have forced AllowPush to false")
	}
}

func TestLoadProject_EnvOverridesProject(t *testing.T) {
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	_ = os.MkdirAll(plumbDir, 0o755)
	_ = os.WriteFile(filepath.Join(plumbDir, "config.toml"),
		[]byte("[edits]\nstrict = false\nrate_limit_per_minute = 30\n"), 0o644)

	t.Setenv("PLUMB_STRICT_EDITS", "1")
	t.Setenv("PLUMB_WRITE_RATE_LIMIT", "5")

	got, err := LoadProject(defaults, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Edits.Strict {
		t.Error("env should have forced Strict to true")
	}
	if got.Edits.RateLimitPerMinute != 5 {
		t.Errorf("env should have set RateLimitPerMinute to 5, got %d", got.Edits.RateLimitPerMinute)
	}
}

func TestDefaults_RefuseHomeRootsEnabled(t *testing.T) {
	if !defaults.Walk.RefuseHomeRoots {
		t.Error("default Walk.RefuseHomeRoots should be true so plumb does not crawl $HOME on a fresh install")
	}
}

func TestDefaults_ReturnsDeepCopy(t *testing.T) {
	got := Defaults()
	got.LSP["go"] = LSPConfig{Command: "mutated"}
	got.LSP["python"] = LSPConfig{
		Command:     "mutated",
		Args:        []string{"changed"},
		RootMarkers: []string{"changed"},
		Env:         map[string]string{"X": "Y"},
	}

	again := Defaults()
	if again.LSP["go"].Command != "gopls" {
		t.Fatalf("mutating Defaults result changed later defaults: %#v", again.LSP["go"])
	}
	if again.LSP["python"].Command != "pyright-langserver" {
		t.Fatalf("mutating Defaults result changed later python config: %#v", again.LSP["python"])
	}
}

func TestLoadProject_ReturnsDeepCopyOfBase(t *testing.T) {
	base := Defaults()
	base.LSP["go"] = LSPConfig{
		Command:     "gopls",
		Args:        []string{"serve"},
		RootMarkers: []string{"go.work", "go.mod"},
		Env:         map[string]string{"A": "B"},
		Enabled:     true,
	}

	got, err := LoadProject(base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gotGo := got.LSP["go"]
	gotGo.Args[0] = "mutated"
	gotGo.RootMarkers[0] = "mutated"
	gotGo.Env["A"] = "mutated"
	got.LSP["go"] = gotGo

	if base.LSP["go"].Args[0] != "serve" {
		t.Fatalf("LoadProject result shared Args with base: %#v", base.LSP["go"].Args)
	}
	if base.LSP["go"].RootMarkers[0] != "go.work" {
		t.Fatalf("LoadProject result shared RootMarkers with base: %#v", base.LSP["go"].RootMarkers)
	}
	if base.LSP["go"].Env["A"] != "B" {
		t.Fatalf("LoadProject result shared Env with base: %#v", base.LSP["go"].Env)
	}
}

func TestLoadProject_OverridesWalk(t *testing.T) {
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[walk]\nrefuse_home_roots = false\n"
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadProject(defaults, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if got.Walk.RefuseHomeRoots {
		t.Error("project config should have set Walk.RefuseHomeRoots to false")
	}
}

func TestApplyEnv_RefuseHomeRoots(t *testing.T) {
	t.Setenv("PLUMB_REFUSE_HOME_ROOTS", "0")
	cfg := defaults
	applyEnv(&cfg)
	if cfg.Walk.RefuseHomeRoots {
		t.Error("PLUMB_REFUSE_HOME_ROOTS=0 should disable the guard")
	}
}

func TestLoadProject_InvalidRateLimitRejected(t *testing.T) {
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	_ = os.MkdirAll(plumbDir, 0o755)
	_ = os.WriteFile(filepath.Join(plumbDir, "config.toml"),
		[]byte("[edits]\nrate_limit_per_minute = -1\n"), 0o644)

	_, err := LoadProject(defaults, ws)
	if err == nil {
		t.Fatal("expected validation error for negative rate limit")
	}
}

func TestDefaults_PostWriteDiagnosticsMs(t *testing.T) {
	if defaults.Edits.PostWriteDiagnosticsMs != 300 {
		t.Errorf("default PostWriteDiagnosticsMs = %d, want 300", defaults.Edits.PostWriteDiagnosticsMs)
	}
}

func TestValidate_PostWriteDiagnosticsMs_NegativeRejected(t *testing.T) {
	cfg := defaults
	cfg.Edits.PostWriteDiagnosticsMs = -1
	if err := validate(cfg); err == nil {
		t.Fatal("expected validation error for negative post_write_diagnostics_ms")
	}
}

func TestValidate_PostWriteDiagnosticsMs_ZeroAllowed(t *testing.T) {
	cfg := defaults
	cfg.Edits.PostWriteDiagnosticsMs = 0
	if err := validate(cfg); err != nil {
		t.Fatalf("zero post_write_diagnostics_ms should be valid (disables polling): %v", err)
	}
}

func TestApplyEnv_PostWriteDiagMs(t *testing.T) {
	t.Setenv("PLUMB_POST_WRITE_DIAG_MS", "1500")
	cfg := defaults
	applyEnv(&cfg)
	if cfg.Edits.PostWriteDiagnosticsMs != 1500 {
		t.Errorf("PLUMB_POST_WRITE_DIAG_MS=1500 not applied, got %d", cfg.Edits.PostWriteDiagnosticsMs)
	}
}

func TestDefaults_LogFormat(t *testing.T) {
	if defaults.LogFormat != "text" {
		t.Errorf("default LogFormat = %q, want \"text\"", defaults.LogFormat)
	}
}

func TestValidate_LogFormat_InvalidRejected(t *testing.T) {
	cfg := defaults
	cfg.LogFormat = "yaml"
	if err := validate(cfg); err == nil {
		t.Fatal("expected validation error for invalid log_format")
	}
}

func TestValidate_LogFormat_JSONAllowed(t *testing.T) {
	cfg := defaults
	cfg.LogFormat = "json"
	if err := validate(cfg); err != nil {
		t.Fatalf("log_format=json should be valid: %v", err)
	}
}

func TestApplyEnv_LogFormat(t *testing.T) {
	t.Setenv("PLUMB_LOG_FORMAT", "json")
	cfg := defaults
	applyEnv(&cfg)
	if cfg.LogFormat != "json" {
		t.Errorf("PLUMB_LOG_FORMAT=json not applied, got %q", cfg.LogFormat)
	}
}

func TestDefaults_WorkspaceAutoAttachDisabled(t *testing.T) {
	if defaults.Workspace.AutoAttach {
		t.Error("default Workspace.AutoAttach should be false — opt-in only")
	}
	if defaults.Workspace.AutoAttachPersist {
		t.Error("default Workspace.AutoAttachPersist should be false — opt-in only")
	}
}

func TestDefaults_TopologyEnabledAndMaxFileSize(t *testing.T) {
	cfg := Defaults()
	if !cfg.Topology.Enabled {
		t.Error("topology must be enabled by default (opt out with [topology] enabled = false)")
	}
	const wantMaxSize = 512 * 1024
	if cfg.Topology.MaxFileSizeBytes != wantMaxSize {
		t.Errorf("MaxFileSizeBytes = %d, want %d", cfg.Topology.MaxFileSizeBytes, wantMaxSize)
	}
}

func TestDefaults_TopologyResyncPacingAndInterval(t *testing.T) {
	cfg := Defaults()
	if cfg.Topology.ResyncBatch != 100 {
		t.Errorf("ResyncBatch = %d, want 100", cfg.Topology.ResyncBatch)
	}
	if cfg.Topology.ResyncPauseMs != 25 {
		t.Errorf("ResyncPauseMs = %d, want 25", cfg.Topology.ResyncPauseMs)
	}
	if cfg.Topology.ResyncIntervalMinutes != 60 {
		t.Errorf("ResyncIntervalMinutes = %d, want 60", cfg.Topology.ResyncIntervalMinutes)
	}
}

func TestDefaults_TopologyExcludePatternsIsolated(t *testing.T) {
	cfg := Defaults()
	cfg.Topology.ExcludePatterns = []string{"vendor/"}
	cfg2 := Defaults()
	if len(cfg2.Topology.ExcludePatterns) > 0 {
		t.Errorf("mutating ExcludePatterns leaked into next Defaults() call: %v", cfg2.Topology.ExcludePatterns)
	}
}

func TestApplyEnv_AutoAttach(t *testing.T) {
	for _, val := range []string{"1", "true", "yes"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("PLUMB_AUTO_ATTACH", val)
			cfg := defaults
			applyEnv(&cfg)
			if !cfg.Workspace.AutoAttach {
				t.Errorf("PLUMB_AUTO_ATTACH=%s should enable AutoAttach", val)
			}
		})
	}
}

func TestApplyEnv_AutoAttachPersist(t *testing.T) {
	t.Setenv("PLUMB_AUTO_ATTACH_PERSIST", "1")
	cfg := defaults
	applyEnv(&cfg)
	if !cfg.Workspace.AutoAttachPersist {
		t.Error("PLUMB_AUTO_ATTACH_PERSIST=1 should enable AutoAttachPersist")
	}
	if !cfg.Workspace.AutoAttach {
		t.Error("PLUMB_AUTO_ATTACH_PERSIST=1 should imply AutoAttach")
	}
}

func TestLoadProject_AutoAttachPersistImpliesAutoAttach(t *testing.T) {
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[workspace]\nauto_attach_persist = true\n"
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadProject(defaults, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if !got.Workspace.AutoAttachPersist {
		t.Fatal("project config should have set Workspace.AutoAttachPersist to true")
	}
	if !got.Workspace.AutoAttach {
		t.Error("Workspace.AutoAttachPersist should imply Workspace.AutoAttach")
	}
}

func TestLoadProject_OverridesWorkspace(t *testing.T) {
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[workspace]\nauto_attach = true\nauto_attach_persist = true\n"
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadProject(defaults, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if !got.Workspace.AutoAttach {
		t.Error("project config should have set Workspace.AutoAttach to true")
	}
	if !got.Workspace.AutoAttachPersist {
		t.Error("project config should have set Workspace.AutoAttachPersist to true")
	}
}

func TestDefaults_UIThemeIsNordico(t *testing.T) {
	cfg := Defaults()
	if cfg.UI.Theme != "nordico" {
		t.Errorf("UI.Theme default = %q, want %q", cfg.UI.Theme, "nordico")
	}
}

func TestSave_PersistsFieldAndPreservesRest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if err := Save(func(c *Config) { c.UI.Theme = "gruvbox" }); err != nil {
		t.Fatalf("Save theme: %v", err)
	}
	if err := Save(func(c *Config) { c.LogLevel = "warn"; c.Edits.Strict = true }); err != nil {
		t.Fatalf("Save edits: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want \"warn\"", got.LogLevel)
	}
	if !got.Edits.Strict {
		t.Error("Edits.Strict should be true after Save")
	}
	// The earlier theme save must survive the second mutation.
	if got.UI.Theme != "gruvbox" {
		t.Errorf("UI.Theme = %q, want preserved \"gruvbox\"", got.UI.Theme)
	}
}

func TestSaveTheme_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if err := SaveTheme("dracula"); err != nil {
		t.Fatalf("SaveTheme: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load after SaveTheme: %v", err)
	}
	if got.UI.Theme != "dracula" {
		t.Errorf("UI.Theme = %q after save, want %q", got.UI.Theme, "dracula")
	}
}

func TestSaveTheme_PreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Write a custom rate limit, then save a theme. The rate limit must survive.
	cfgPath := GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("[edits]\nrate_limit_per_minute = 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SaveTheme("gruvbox"); err != nil {
		t.Fatalf("SaveTheme: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.UI.Theme != "gruvbox" {
		t.Errorf("UI.Theme = %q, want %q", got.UI.Theme, "gruvbox")
	}
	if got.Edits.RateLimitPerMinute != 42 {
		t.Errorf("RateLimitPerMinute = %d after SaveTheme, want 42", got.Edits.RateLimitPerMinute)
	}
}

// TestSaveTheme_RefusesBrokenConfig verifies SaveTheme aborts rather than
// overwriting a config file that exists but cannot be parsed — so the user's
// recoverable settings are not silently replaced with defaults.
func TestSaveTheme_RefusesBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfgPath := GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := []byte("[edits\nrate_limit_per_minute = 42\n") // unterminated table header
	if err := os.WriteFile(cfgPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SaveTheme("dracula"); err == nil {
		t.Fatal("SaveTheme succeeded on a broken config; want an error")
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(broken) {
		t.Errorf("SaveTheme overwrote a broken config; file changed to:\n%s", after)
	}
}
