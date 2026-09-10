package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckConfigs_WarnsOnFrozenDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	cfgDir := filepath.Join(home, ".config", "plumb")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}

	// 1. Clean custom config
	cleanConfig := `
theme = "dark"

[edits]
strict = true

[git]
protected_branches = ["main", "master", "develop"]
`
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cleanConfig), 0o600); err != nil {
		t.Fatalf("writing clean config: %v", err)
	}

	results := checkConfigs("")
	for _, r := range results {
		if r.name == "frozen defaults" {
			t.Errorf("clean config must not report frozen defaults, got %+v", r)
		}
	}

	// 2. Config with frozen defaults
	frozenConfig := `
theme = "dark"
command = []

[git]
protected_branches = ["main", "master"]

[quality]
analysers = ["golangci-lint"]
`
	if err := os.WriteFile(cfgPath, []byte(frozenConfig), 0o600); err != nil {
		t.Fatalf("writing frozen config: %v", err)
	}

	results = checkConfigs("")
	var found *checkResult
	for i := range results {
		if results[i].name == "frozen defaults" {
			found = &results[i]
			break
		}
	}

	if found == nil {
		t.Fatalf("checkConfigs did not return a 'frozen defaults' result; got: %+v", results)
	}
	if !found.ok || !found.warn {
		t.Errorf("frozen defaults result should be ok=true, warn=true; got ok=%v, warn=%v", found.ok, found.warn)
	}
	if !strings.Contains(found.detail, "command") || !strings.Contains(found.detail, "git.protected_branches") {
		t.Errorf("expected frozen keys in detail, got: %q", found.detail)
	}
	if !strings.Contains(found.fix, "delete redundant default lines") {
		t.Errorf("expected fix instruction in fix field, got: %q", found.fix)
	}
}
