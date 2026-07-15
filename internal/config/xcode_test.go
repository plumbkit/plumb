package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestXcodeDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Xcode.AutoBuildServer {
		t.Fatal("xcode auto build server defaulted on")
	}
	if cfg.Xcode.Scheme != "" {
		t.Fatalf("scheme = %q; want empty", cfg.Xcode.Scheme)
	}
	if cfg.Xcode.Timeout.Duration != 2*time.Minute {
		t.Fatalf("timeout = %s; want 2m", cfg.Xcode.Timeout.Duration)
	}
}

func TestApplyEnvXcodeAutoBuildServer(t *testing.T) {
	t.Setenv("PLUMB_XCODE_AUTO_BUILD_SERVER", "true")
	cfg := Defaults()
	applyEnv(&cfg)
	if !cfg.Xcode.AutoBuildServer {
		t.Fatal("PLUMB_XCODE_AUTO_BUILD_SERVER was not applied")
	}
}

func TestLoadProjectOverridesXcode(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("[xcode]\nauto_build_server = true\nscheme = \"App\"\ntimeout = \"30s\"\n")
	if err := os.WriteFile(filepath.Join(root, ".plumb", "config.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProject(Defaults(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Xcode.AutoBuildServer || cfg.Xcode.Scheme != "App" || cfg.Xcode.Timeout.Duration != 30*time.Second {
		t.Fatalf("xcode config = %#v", cfg.Xcode)
	}
}

func TestValidateXcodeTimeout(t *testing.T) {
	cfg := Defaults()
	cfg.Xcode.AutoBuildServer = true
	cfg.Xcode.Timeout.Duration = 0
	if err := validate(cfg); err == nil {
		t.Fatal("zero xcode timeout accepted")
	}
}

func TestXcodeFieldsReloadNextSession(t *testing.T) {
	for _, key := range []string{"xcode.auto_build_server", "xcode.scheme", "xcode.timeout"} {
		field, ok := Lookup(key)
		if !ok {
			t.Fatalf("%s missing from registry", key)
		}
		if field.ReloadTier != ReloadNextSession {
			t.Fatalf("%s reload tier = %s", key, field.ReloadTier)
		}
	}
}
