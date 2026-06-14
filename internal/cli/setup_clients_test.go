package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNewClientConfigPaths(t *testing.T) {
	cases := []struct {
		name       string
		fn         func() (string, error)
		wantBase   string
		wantParent string
	}{
		{"cursor", CursorConfigPath, "mcp.json", ".cursor"},
		{"augment", AugmentConfigPath, "settings.json", ".augment"},
		{"qwen", QwenConfigPath, "settings.json", ".qwen"},
		{"antigravity", AntigravityConfigPath, "mcp_config.json", "antigravity-cli"},
		{"opencode", OpenCodeConfigPath, "opencode.json", "opencode"},
		{"crush", CrushConfigPath, "crush.json", "crush"},
		{"goose", GooseConfigPath, "config.yaml", "goose"},
		{"hermes", HermesConfigPath, "config.yaml", ".hermes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, err := tc.fn()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if filepath.Base(path) != tc.wantBase {
				t.Errorf("base: got %q, want %q", filepath.Base(path), tc.wantBase)
			}
			if got := filepath.Base(filepath.Dir(path)); got != tc.wantParent {
				t.Errorf("parent dir: got %q, want %q", got, tc.wantParent)
			}
		})
	}
}

func TestWriteYAML_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.yaml")

	m := map[string]any{"extensions": map[string]any{"plumb": map[string]any{"cmd": "plumb"}}}
	if err := writeYAML(path, m); err != nil {
		t.Fatalf("writeYAML: %v", err)
	}

	got, _, err := readOrInitYAMLConfig(path)
	if err != nil {
		t.Fatalf("readOrInitYAMLConfig: %v", err)
	}
	ext, ok := got["extensions"].(map[string]any)
	if !ok {
		t.Fatalf("extensions is not a map: %T", got["extensions"])
	}
	plumb, ok := ext["plumb"].(map[string]any)
	if !ok {
		t.Fatalf("plumb entry is not a map: %T", ext["plumb"])
	}
	if plumb["cmd"] != "plumb" {
		t.Errorf("cmd: got %v", plumb["cmd"])
	}
}

func TestSetupOpenCodeInto_FreshConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")

	added, preserved, err := setupOpenCodeInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupOpenCodeInto: %v", err)
	}
	if !added {
		t.Error("expected added=true for fresh config")
	}
	if len(preserved) != 0 {
		t.Errorf("expected no preserved servers, got %v", preserved)
	}

	result, _, err := readOrInitClaudeConfig(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	plumb := result["mcp"].(map[string]any)["plumb"].(map[string]any)
	if plumb["type"] != "local" {
		t.Errorf("type: got %v, want local", plumb["type"])
	}
	if plumb["enabled"] != true {
		t.Errorf("enabled: got %v, want true", plumb["enabled"])
	}
	want := []any{"/usr/local/bin/plumb", "serve"}
	if !reflect.DeepEqual(plumb["command"], want) {
		t.Errorf("command: got %v, want %v", plumb["command"], want)
	}
}

func TestSetupOpenCodeInto_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")

	if _, _, err := setupOpenCodeInto(path, "/usr/local/bin/plumb"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	added, _, err := setupOpenCodeInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if added {
		t.Error("expected added=false on second run (already registered)")
	}
}

func TestSetupCrushInto_FreshConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crush.json")

	added, _, err := setupCrushInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupCrushInto: %v", err)
	}
	if !added {
		t.Error("expected added=true for fresh config")
	}

	result, _, err := readOrInitClaudeConfig(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	plumb := result["mcp"].(map[string]any)["plumb"].(map[string]any)
	if plumb["type"] != "stdio" {
		t.Errorf("type: got %v, want stdio", plumb["type"])
	}
	if plumb["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command: got %v", plumb["command"])
	}
	if !reflect.DeepEqual(plumb["args"], []any{"serve"}) {
		t.Errorf("args: got %v, want [serve]", plumb["args"])
	}
}

func TestSetupGooseInto_FreshConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	added, _, err := setupGooseInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupGooseInto: %v", err)
	}
	if !added {
		t.Error("expected added=true for fresh config")
	}

	result, _, err := readOrInitYAMLConfig(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	plumb := result["extensions"].(map[string]any)["plumb"].(map[string]any)
	if plumb["type"] != "stdio" {
		t.Errorf("type: got %v, want stdio", plumb["type"])
	}
	if plumb["cmd"] != "/usr/local/bin/plumb" {
		t.Errorf("cmd: got %v", plumb["cmd"])
	}
	if plumb["name"] != "plumb" {
		t.Errorf("name: got %v, want plumb", plumb["name"])
	}
	if plumb["enabled"] != true {
		t.Errorf("enabled: got %v, want true", plumb["enabled"])
	}
}

func TestSetupGooseInto_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if _, _, err := setupGooseInto(path, "/usr/local/bin/plumb"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	added, _, err := setupGooseInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if added {
		t.Error("expected added=false on second run (already registered)")
	}
}

func TestSetupHermesInto_FreshConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	added, _, err := setupHermesInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupHermesInto: %v", err)
	}
	if !added {
		t.Error("expected added=true for fresh config")
	}

	result, _, err := readOrInitYAMLConfig(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	plumb := result["mcp_servers"].(map[string]any)["plumb"].(map[string]any)
	if plumb["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command: got %v", plumb["command"])
	}
	if !reflect.DeepEqual(plumb["args"], []any{"serve"}) {
		t.Errorf("args: got %v, want [serve]", plumb["args"])
	}
}

func TestSetupHermesInto_MergesNonDestructively(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	existing := "mcp_servers:\n  other:\n    command: other-bin\n    args: []\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	added, preserved, err := setupHermesInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupHermesInto: %v", err)
	}
	if !added {
		t.Error("expected added=true")
	}
	if len(preserved) != 1 || preserved[0] != "other" {
		t.Errorf("expected preserved=[other], got %v", preserved)
	}

	result, _, err := readOrInitYAMLConfig(path)
	if err != nil {
		t.Fatalf("re-reading config: %v", err)
	}
	servers := result["mcp_servers"].(map[string]any)
	if servers["other"] == nil {
		t.Error("pre-existing 'other' server was removed")
	}
	if servers["plumb"] == nil {
		t.Error("plumb server not found after merge")
	}

	entries, _ := os.ReadDir(dir)
	var backups int
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".bak" {
			backups++
		}
	}
	if backups == 0 {
		t.Error("expected a .bak backup before modifying existing config")
	}
}
