package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
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
	if filepath.Base(path) != "settings.json" {
		t.Errorf("unexpected filename: %s", filepath.Base(path))
	}
}

func TestCodexConfigPath_UsesCodexHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)

	path, err := CodexConfigPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(dir, "config.toml")
	if path != want {
		t.Fatalf("path: got %q, want %q", path, want)
	}
}

func TestCodexConfigPath_NoError(t *testing.T) {
	t.Setenv("CODEX_HOME", "")

	path, err := CodexConfigPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}
	if filepath.Base(path) != "config.toml" {
		t.Errorf("unexpected filename: %s", filepath.Base(path))
	}
	if filepath.Base(filepath.Dir(path)) != ".codex" {
		t.Errorf("unexpected config directory: %s", filepath.Dir(path))
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

func TestSetupCodexInto_FreshConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	added, preserved, err := setupCodexInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupCodexInto: %v", err)
	}
	if !added {
		t.Error("expected added=true for fresh config")
	}
	if len(preserved) != 0 {
		t.Errorf("expected no preserved servers, got %v", preserved)
	}

	result, _, err := readOrInitCodexConfig(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}

	servers := result["mcp_servers"].(map[string]any)
	plumb := servers["plumb"].(map[string]any)

	if plumb["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command: got %v", plumb["command"])
	}
	if !stringSliceEqual(plumb["args"], []string{"serve"}) {
		t.Errorf("args: got %v, want [serve]", plumb["args"])
	}
}

func TestSetupCodexInto_MergesNonDestructively(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	existing := `[mcp_servers.other]
command = "other-bin"
args = []
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	added, preserved, err := setupCodexInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupCodexInto: %v", err)
	}
	if !added {
		t.Error("expected added=true")
	}
	if len(preserved) != 1 || preserved[0] != "other" {
		t.Errorf("expected preserved=[other], got %v", preserved)
	}

	result, _, err := readOrInitCodexConfig(path)
	if err != nil {
		t.Fatalf("readOrInitCodexConfig after write: %v", err)
	}
	merged := result["mcp_servers"].(map[string]any)
	if merged["other"] == nil {
		t.Error("pre-existing 'other' server was removed")
	}
	if merged["plumb"] == nil {
		t.Error("plumb server not found after merge")
	}

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

func TestSetupCodexInto_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	added, _, err := setupCodexInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("run 0: %v", err)
	}
	if !added {
		t.Error("expected added=true on first run")
	}

	for i := 1; i < 3; i++ {
		added, _, err := setupCodexInto(path, "/usr/local/bin/plumb")
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if added {
			t.Errorf("run %d: expected added=false (already registered)", i)
		}
	}

	result, _, err := readOrInitCodexConfig(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	servers := result["mcp_servers"].(map[string]any)
	if len(servers) != 1 {
		t.Errorf("expected exactly 1 server after 3 runs, got %d", len(servers))
	}
}

func TestSetupCodexInto_InvalidMCPServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	bad := `mcp_servers = "not-an-object"`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := setupCodexInto(path, "/usr/local/bin/plumb")
	if err == nil {
		t.Fatal("expected error for invalid mcp_servers type, got nil")
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

// --- skill helpers ---

func TestClaudeSkillsDir_NoError(t *testing.T) {
	path, err := claudeSkillsDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}
	if filepath.Base(path) != "skills" {
		t.Errorf("expected 'skills' dir name, got %q", filepath.Base(path))
	}
	if filepath.Base(filepath.Dir(path)) != ".claude" {
		t.Errorf("expected '.claude' parent, got %q", filepath.Base(filepath.Dir(path)))
	}
}

func TestInstallSkill_Fresh(t *testing.T) {
	dir := t.TempDir()
	content := "---\nname: test-skill\ndescription: A test skill\n---\nbody\n"

	action, err := installSkill(dir, "test-skill", content)
	if err != nil {
		t.Fatalf("installSkill: %v", err)
	}
	if action != "installed" {
		t.Errorf("action: got %q, want %q", action, "installed")
	}

	got, err := os.ReadFile(filepath.Join(dir, "test-skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading SKILL.md: %v", err)
	}
	if string(got) != content {
		t.Errorf("content mismatch: got %q, want %q", string(got), content)
	}
}

func TestInstallSkill_Idempotent(t *testing.T) {
	dir := t.TempDir()
	content := "---\nname: test-skill\ndescription: test\n---\n"

	if _, err := installSkill(dir, "test-skill", content); err != nil {
		t.Fatalf("first install: %v", err)
	}

	action, err := installSkill(dir, "test-skill", content)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if action != "unchanged" {
		t.Errorf("action: got %q, want %q", action, "unchanged")
	}
}

func TestInstallSkill_Updated(t *testing.T) {
	dir := t.TempDir()
	old := "---\nname: test-skill\ndescription: test\n---\nold content\n"
	new := "---\nname: test-skill\ndescription: test\n---\nnew content\n"

	if _, err := installSkill(dir, "test-skill", old); err != nil {
		t.Fatalf("first install: %v", err)
	}

	action, err := installSkill(dir, "test-skill", new)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if action != "updated" {
		t.Errorf("action: got %q, want %q", action, "updated")
	}

	// Backup must have been created.
	entries, _ := os.ReadDir(filepath.Join(dir, "test-skill"))
	var backups []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".bak" {
			backups = append(backups, e.Name())
		}
	}
	if len(backups) == 0 {
		t.Error("expected a .bak backup file to be created before update")
	}

	// New content must be installed.
	got, err := os.ReadFile(filepath.Join(dir, "test-skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading SKILL.md after update: %v", err)
	}
	if string(got) != new {
		t.Errorf("content after update: got %q, want %q", string(got), new)
	}
}

func TestClaudeCodeSkills_HaveValidFrontmatter(t *testing.T) {
	skills := claudeCodeSkills()
	if len(skills) == 0 {
		t.Fatal("expected at least one embedded skill")
	}

	// Pin the expected skill set so a new skills/<name>/ directory added
	// without an embedded SKILL.md — or one accidentally dropped — is
	// caught here rather than silently missing at install time.
	wantNames := []string{"plumb-explore", "plumb-minimal-change", "plumb-refactor"}
	gotNames := make([]string, 0, len(skills))
	for _, skill := range skills {
		gotNames = append(gotNames, skill.Name)
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("skill set: got %v, want %v", gotNames, wantNames)
	}

	for _, skill := range skills {
		if skill.Name == "" {
			t.Error("skill with empty name")
		}
		if !strings.Contains(skill.Content, "name: "+skill.Name) {
			t.Errorf("skill %q: missing 'name: %s' frontmatter", skill.Name, skill.Name)
		}
		if !strings.Contains(skill.Content, "description:") {
			t.Errorf("skill %q: missing 'description:' frontmatter", skill.Name)
		}
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
