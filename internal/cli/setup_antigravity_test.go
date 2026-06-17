package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeLegacyAntigravity writes a flat mcp_config.json registering plumb at bin
// under <base>/<dir>/mcp_config.json, creating parents.
func writeLegacyAntigravity(t *testing.T, base, dir, bin string) string {
	t.Helper()
	d := filepath.Join(base, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "mcp_config.json")
	cfg := map[string]any{"mcpServers": map[string]any{
		"plumb": map[string]any{"command": bin, "args": []any{"serve"}},
	}}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnsureFlatAntigravityConfig_RepointsStaleAndPreservesSiblings(t *testing.T) {
	base := t.TempDir()
	p := filepath.Join(base, "mcp_config.json")
	cfg := map[string]any{"mcpServers": map[string]any{
		"plumb": map[string]any{"command": "/old/plumb", "args": []any{"serve"}},
		"other": map[string]any{"command": "/usr/bin/other"},
	}}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := ensureFlatAntigravityConfig(p, "/new/plumb")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected ensureFlatAntigravityConfig to report a change")
	}

	var got map[string]any
	raw, _ := os.ReadFile(p)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	servers := got["mcpServers"].(map[string]any)
	if cmd := servers["plumb"].(map[string]any)["command"]; cmd != "/new/plumb" {
		t.Errorf("plumb command: got %v, want /new/plumb", cmd)
	}
	if other, ok := servers["other"].(map[string]any); !ok || other["command"] != "/usr/bin/other" {
		t.Errorf("sibling server not preserved: %v", servers["other"])
	}
	if args := servers["plumb"].(map[string]any)["args"]; !reflect.DeepEqual(args, []any{"serve"}) {
		t.Errorf("existing args not preserved: %v", args)
	}
}

// TestEnsureFlatAntigravityConfig_CreatesWhenAbsent is the core regression:
// setup must CREATE the flat mcp_config.json Antigravity reads, not only repoint
// a pre-existing one. The earlier standalone-only setup left a fresh install
// with no plumb entry in any file Antigravity loads.
func TestEnsureFlatAntigravityConfig_CreatesWhenAbsent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config", "mcp_config.json")

	changed, err := ensureFlatAntigravityConfig(p, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected a fresh flat config to be created")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("flat config not created: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	plumb := got["mcpServers"].(map[string]any)["plumb"].(map[string]any)
	if plumb["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command: got %v, want /usr/local/bin/plumb", plumb["command"])
	}
	if !reflect.DeepEqual(plumb["args"], []any{"serve"}) {
		t.Errorf("args: got %v, want [serve]", plumb["args"])
	}
}

func TestEnsureFlatAntigravityConfig_Idempotent(t *testing.T) {
	base := t.TempDir()
	cur := writeLegacyAntigravity(t, base, "config", "/usr/local/bin/plumb")
	changed, err := ensureFlatAntigravityConfig(cur, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("expected no change when already pointing at the target binary")
	}
}

// TestEnsureAntigravityFlatConfigs_CreatesCanonicalAndExistingSurfaces verifies
// the canonical config/ file is always created, an already-present per-surface
// dir is also written, and a surface dir that doesn't exist is left untouched
// (plumb never materialises an uninstalled Antigravity product's directory).
func TestEnsureAntigravityFlatConfigs_CreatesCanonicalAndExistingSurfaces(t *testing.T) {
	base := t.TempDir()
	// antigravity-cli/ exists (installed); config/ and antigravity-ide/ do not yet.
	if err := os.MkdirAll(filepath.Join(base, "antigravity-cli"), 0o755); err != nil {
		t.Fatal(err)
	}

	changed := ensureAntigravityFlatConfigs(base, "/usr/local/bin/plumb")

	// config/ (canonical, created) + antigravity-cli/ (existing) written.
	for _, d := range []string{"config", "antigravity-cli"} {
		raw, err := os.ReadFile(filepath.Join(base, d, "mcp_config.json"))
		if err != nil {
			t.Fatalf("%s: expected a flat config: %v", d, err)
		}
		var got map[string]any
		_ = json.Unmarshal(raw, &got)
		cmd := got["mcpServers"].(map[string]any)["plumb"].(map[string]any)["command"]
		if cmd != "/usr/local/bin/plumb" {
			t.Errorf("%s: command not set: %v", d, cmd)
		}
	}
	// antigravity-ide/ did not exist, so plumb must not have created it.
	if _, err := os.Stat(filepath.Join(base, "antigravity-ide")); !os.IsNotExist(err) {
		t.Errorf("antigravity-ide/ should not have been created (uninstalled surface)")
	}
	if len(changed) != 2 {
		t.Errorf("expected 2 changed paths (config + antigravity-cli), got %d: %v", len(changed), changed)
	}
}

// TestSetupAntigravityInto_RepointsLegacyConfigs exercises the real ~/.gemini
// layout: the standalone CLI config plus a stale flat config the standalone-only
// setup historically ignored. Setup must repoint the legacy file and report a
// change even when the standalone file is already current.
func TestSetupAntigravityInto_RepointsLegacyConfigs(t *testing.T) {
	gemini := t.TempDir()
	cliStandalone := filepath.Join(gemini, "antigravity-cli", "mcp", "plumb.json")
	staleLegacy := writeLegacyAntigravity(t, gemini, "config", "/old/plumb")

	// Pre-stage the standalone config as already current, so the only thing left
	// to fix is the stale legacy file — setup must still report a change.
	if err := os.MkdirAll(filepath.Dir(cliStandalone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAntigravityConfig(cliStandalone, "/usr/local/bin/plumb"); err != nil {
		t.Fatal(err)
	}

	added, _, err := setupAntigravityInto(cliStandalone, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !added {
		t.Error("expected added=true: a stale legacy config still needed repointing")
	}

	raw, _ := os.ReadFile(staleLegacy)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	cmd := got["mcpServers"].(map[string]any)["plumb"].(map[string]any)["command"]
	if cmd != "/usr/local/bin/plumb" {
		t.Errorf("legacy config not repointed: got %v", cmd)
	}
}

func TestGeminiBaseFromStandalone(t *testing.T) {
	got := geminiBaseFromStandalone(filepath.Join("/home/u", ".gemini", "antigravity-cli", "mcp", "plumb.json"))
	want := filepath.Join("/home/u", ".gemini")
	if got != want {
		t.Errorf("geminiBaseFromStandalone: got %q, want %q", got, want)
	}
}

func TestCheckLegacyAntigravityConfigs(t *testing.T) {
	self := filepath.Join(t.TempDir(), "plumb-current")
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("omitted when no legacy config registers plumb", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if _, ok := checkLegacyAntigravityConfigs(self); ok {
			t.Error("expected the check to be omitted with no legacy plumb configs")
		}
	})

	t.Run("missing binary is a failure", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeLegacyAntigravity(t, filepath.Join(home, ".gemini"), "config", filepath.Join(home, "gone", "plumb"))
		res, ok := checkLegacyAntigravityConfigs(self)
		if !ok || res.ok {
			t.Fatalf("expected a failing result, got ok=%v res.ok=%v", ok, res.ok)
		}
	})

	t.Run("present but mismatched binary is a warning", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		other := filepath.Join(home, "other-plumb")
		if err := os.WriteFile(other, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeLegacyAntigravity(t, filepath.Join(home, ".gemini"), "config", other)
		res, ok := checkLegacyAntigravityConfigs(self)
		if !ok || !res.ok || !res.warn {
			t.Fatalf("expected a warning, got ok=%v res=%+v", ok, res)
		}
	})

	t.Run("current binary is a clean pass", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeLegacyAntigravity(t, filepath.Join(home, ".gemini"), "antigravity-cli", self)
		res, ok := checkLegacyAntigravityConfigs(self)
		if !ok || !res.ok || res.warn {
			t.Fatalf("expected a clean pass, got ok=%v res=%+v", ok, res)
		}
	})
}
