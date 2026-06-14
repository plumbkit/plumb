package cli

import (
	"encoding/json"
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
		{"antigravity", AntigravityConfigPath, "plumb.json", "mcp"},
		{"antigravity-desktop", AntigravityDesktopConfigPath, "plumb.json", "mcp"},
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

// readAntigravityEntry reads a standalone Antigravity config file back as a
// generic map for assertions.
func readAntigravityEntry(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshalling %s: %v", path, err)
	}
	return m
}

func TestSetupAntigravityInto_FreshConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp", "plumb.json")

	added, preserved, err := setupAntigravityInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupAntigravityInto: %v", err)
	}
	if !added {
		t.Error("expected added=true for fresh config")
	}
	if len(preserved) != 0 {
		t.Errorf("expected no preserved servers, got %v", preserved)
	}

	// Antigravity uses a standalone {command, args} entry — NOT an mcpServers wrapper.
	m := readAntigravityEntry(t, path)
	if m["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command: got %v, want /usr/local/bin/plumb", m["command"])
	}
	if !reflect.DeepEqual(m["args"], []any{"serve"}) {
		t.Errorf("args: got %v, want [serve]", m["args"])
	}
	if _, hasWrapper := m["mcpServers"]; hasWrapper {
		t.Error("entry must be a standalone {command,args} object, not an mcpServers wrapper")
	}
}

func TestSetupAntigravityInto_PreservesSiblingsAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	mcpDir := filepath.Join(dir, "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A sibling server file and an existing (stale) plumb.json.
	if err := os.WriteFile(filepath.Join(mcpDir, "other.json"), []byte(`{"command":"other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(mcpDir, "plumb.json")
	if err := os.WriteFile(path, []byte(`{"command":"/old/plumb","args":["serve"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	added, preserved, err := setupAntigravityInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupAntigravityInto: %v", err)
	}
	if !added {
		t.Error("expected added=true when overwriting a stale plumb.json")
	}
	if len(preserved) != 1 || preserved[0] != "other" {
		t.Errorf("expected preserved=[other], got %v", preserved)
	}

	if m := readAntigravityEntry(t, path); m["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command not updated: got %v", m["command"])
	}
	if _, err := os.Stat(filepath.Join(mcpDir, "other.json")); err != nil {
		t.Errorf("sibling other.json was removed: %v", err)
	}

	entries, _ := os.ReadDir(mcpDir)
	var backups int
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".bak" {
			backups++
		}
	}
	if backups == 0 {
		t.Error("expected a .bak backup before overwriting an existing config")
	}
}

func TestSetupAntigravityInto_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp", "plumb.json")

	if _, _, err := setupAntigravityInto(path, "/usr/local/bin/plumb"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	added, _, err := setupAntigravityInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if added {
		t.Error("expected added=false on second run (already registered)")
	}
}

func TestSetupAntigravityInto_MirrorsDesktopToIde(t *testing.T) {
	dir := t.TempDir()
	// Desktop layout: <root>/antigravity/mcp/plumb.json mirrors to
	// <root>/antigravity-ide/mcp/plumb.json, but only when the ide mcp dir exists.
	desktopPath := filepath.Join(dir, "antigravity", "mcp", "plumb.json")
	ideDir := filepath.Join(dir, "antigravity-ide", "mcp")
	if err := os.MkdirAll(ideDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, _, err := setupAntigravityInto(desktopPath, "/usr/local/bin/plumb"); err != nil {
		t.Fatalf("setupAntigravityInto: %v", err)
	}

	idePath := filepath.Join(ideDir, "plumb.json")
	if m := readAntigravityEntry(t, idePath); m["command"] != "/usr/local/bin/plumb" {
		t.Errorf("ide mirror not written correctly: got %v", m["command"])
	}
}

func TestSetupAntigravityInto_NoMirrorWhenIdeDirAbsent(t *testing.T) {
	dir := t.TempDir()
	desktopPath := filepath.Join(dir, "antigravity", "mcp", "plumb.json")

	if _, _, err := setupAntigravityInto(desktopPath, "/usr/local/bin/plumb"); err != nil {
		t.Fatalf("setupAntigravityInto: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "antigravity-ide", "mcp", "plumb.json")); !os.IsNotExist(err) {
		t.Errorf("ide mirror should not be created when the ide mcp dir is absent (err=%v)", err)
	}
}
