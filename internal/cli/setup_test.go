package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestWriteJSON_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	m := map[string]any{"key": "value", "num": float64(42)}
	if err := writeJSON(path, m); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["key"] != "value" {
		t.Errorf("key: got %v", got["key"])
	}
}

func TestReadOrInitClaudeConfig_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "cfg.json")

	m, isNew, err := readOrInitClaudeConfig(path)
	if err != nil {
		t.Fatalf("unexpected error for missing file: %v", err)
	}
	if m == nil {
		t.Fatal("expected empty map, got nil")
	}
	if !isNew {
		t.Error("expected isNew=true for missing file")
	}
	// Directory should have been created.
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestReadOrInitClaudeConfig_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	existing := `{"mcpServers":{"other":{"command":"other-bin"}}}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	m, isNew, err := readOrInitClaudeConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["mcpServers"] == nil {
		t.Fatal("mcpServers should be present")
	}
	if isNew {
		t.Error("expected isNew=false for existing file")
	}
}

func TestReadOrInitClaudeConfig_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	m, isNew, err := readOrInitClaudeConfig(path)
	if err != nil {
		t.Fatalf("unexpected error for empty file: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
	if isNew {
		t.Error("expected isNew=false for existing empty file")
	}
}

func TestGeminiConfigPath_NoError(t *testing.T) {
	path, err := GeminiConfigPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}
	if filepath.Base(path) != "mcp_config.json" {
		t.Errorf("unexpected filename: %s", filepath.Base(path))
	}
}

func TestSetupClaudeDesktopInto_FreshConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")

	added, preserved, err := setupClaudeDesktopInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupClaudeDesktopInto: %v", err)
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

	servers := result["mcpServers"].(map[string]any)
	plumb := servers["plumb"].(map[string]any)

	if plumb["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command: got %v", plumb["command"])
	}

	// args must be exactly ["serve"] — no --root or other flags.
	args, ok := plumb["args"].([]any)
	if !ok {
		t.Fatalf("args is not a slice: %T", plumb["args"])
	}
	want := []any{"serve"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args: got %v, want %v", args, want)
	}
}

func TestSetupClaudeDesktopInto_MergesNonDestructively(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")

	// Pre-existing config with another server.
	existing := map[string]any{
		"mcpServers": map[string]any{
			"other": map[string]any{"command": "other-bin", "args": []string{}},
		},
	}
	data, _ := json.Marshal(existing)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	added, preserved, err := setupClaudeDesktopInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupClaudeDesktopInto: %v", err)
	}
	if !added {
		t.Error("expected added=true")
	}
	if len(preserved) != 1 || preserved[0] != "other" {
		t.Errorf("expected preserved=[other], got %v", preserved)
	}

	result, _, err := readOrInitClaudeConfig(path)
	if err != nil {
		t.Fatalf("readOrInitClaudeConfig after write: %v", err)
	}
	merged := result["mcpServers"].(map[string]any)
	if merged["other"] == nil {
		t.Error("pre-existing 'other' server was removed")
	}
	if merged["plumb"] == nil {
		t.Error("plumb server not found after merge")
	}
	// Backup must have been created.
	entries, _ := os.ReadDir(dir)
	var backups []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".bak" {
			backups = append(backups, e.Name())
		}
	}
	if len(backups) == 0 {
		t.Error("expected a .bak backup file to be created before modifying existing config")
	}
}

func TestSetupClaudeDesktopInto_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")

	// First call: registers plumb (added=true).
	added, _, err := setupClaudeDesktopInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("run 0: %v", err)
	}
	if !added {
		t.Error("expected added=true on first run")
	}

	// Subsequent calls with the same binary: no-op (added=false).
	for i := 1; i < 3; i++ {
		added, _, err := setupClaudeDesktopInto(path, "/usr/local/bin/plumb")
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if added {
			t.Errorf("run %d: expected added=false (already registered)", i)
		}
	}

	result, _, err := readOrInitClaudeConfig(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	servers := result["mcpServers"].(map[string]any)
	if len(servers) != 1 {
		t.Errorf("expected exactly 1 server after 3 runs, got %d", len(servers))
	}
}

func TestSetupClaudeDesktopInto_InvalidMcpServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")

	// mcpServers is a string, not an object.
	bad := `{"mcpServers": "not-an-object"}`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := setupClaudeDesktopInto(path, "/usr/local/bin/plumb")
	if err == nil {
		t.Fatal("expected error for invalid mcpServers type, got nil")
	}
}

func TestClaudeDesktopConfigPath_NoError(t *testing.T) {
	path, err := claudeDesktopConfigPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}
	if filepath.Base(path) != "claude_desktop_config.json" {
		t.Errorf("unexpected filename: %s", filepath.Base(path))
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		if filepath.Base(filepath.Dir(path)) != "Claude" {
			t.Errorf("expected Claude directory, got %s", filepath.Dir(path))
		}
	}
}
