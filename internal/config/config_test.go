package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
