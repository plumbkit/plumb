package config

import (
	"os"
	"strings"
	"testing"
)

// TestSave_DoesNotBakeEnvOverride verifies a full-struct Save does not persist
// an active PLUMB_* environment override into config.toml as if the user had
// chosen it (which would outlive the env var). Regression test for config-3.
func TestSave_DoesNotBakeEnvOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PLUMB_LOG_LEVEL", "debug") // a transient environment override

	if err := Save(func(c *Config) { c.LogFormat = "json" }); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(GlobalConfigPath())
	if err != nil {
		t.Fatalf("reading written config: %v", err)
	}
	got := string(data)

	// The deliberate change is persisted.
	if !strings.Contains(got, "json") {
		t.Errorf("Save did not persist the log_format change:\n%s", got)
	}
	// The transient env override must NOT be baked into the file.
	if strings.Contains(got, "debug") {
		t.Errorf("Save baked the PLUMB_LOG_LEVEL=debug env override into config.toml:\n%s", got)
	}
}
