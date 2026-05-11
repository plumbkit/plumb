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
