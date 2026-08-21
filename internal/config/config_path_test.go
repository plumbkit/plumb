package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A relative XDG_CONFIG_HOME is invalid per the XDG basedir spec ("If an
// implementation encounters a relative path it must be considered invalid")
// and must be ignored rather than joined against the process cwd.
//
// This used to be the one place in plumb that read the variable raw. The
// divergence was visible on a live binary: `plumb config show` reported the
// config directory as ~/.config/plumb (internal/paths ignores the relative
// value, correctly) while the loader read <cwd>/<rel>/plumb/config.toml. Worse,
// the daemon chdirs to / , so the CLI and the daemon resolved the same setting
// to different files.
func TestLegacyConfigPath_IgnoresRelativeXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "relative/config")

	got := legacyConfigPath()
	if want := filepath.Join(home, ".config", "plumb", "config.toml"); got != want {
		t.Fatalf("legacyConfigPath() = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("legacyConfigPath() returned a cwd-dependent path: %q", got)
	}
}

func TestLegacyConfigPath_HonoursAbsoluteXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	if got, want := legacyConfigPath(), filepath.Join(xdg, "plumb", "config.toml"); got != want {
		t.Fatalf("legacyConfigPath() = %q, want %q", got, want)
	}
}

func TestLegacyConfigPath_FallsBackToDotConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	if got, want := legacyConfigPath(), filepath.Join(home, ".config", "plumb", "config.toml"); got != want {
		t.Fatalf("legacyConfigPath() = %q, want %q", got, want)
	}
}

// The legacy path is a READ fallback only: it is used when the current location
// has no config yet and the legacy one does. A relative XDG_CONFIG_HOME must
// not be able to smuggle a config in via that route either.
func TestConfigPath_RelativeXDGConfigHomeDoesNotDivertTheLoader(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "relative/config")
	// Plant a config where the relative value would have pointed, relative to
	// this test's working directory.
	planted := filepath.Join("relative", "config", "plumb")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll("relative") })
	if err := os.WriteFile(filepath.Join(planted, "config.toml"), []byte("[topology]\nenabled = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := configPath(); !filepath.IsAbs(got) {
		t.Fatalf("configPath() resolved to a cwd-dependent path: %q", got)
	}
}
